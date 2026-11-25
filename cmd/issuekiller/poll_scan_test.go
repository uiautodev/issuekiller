package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The reaction, PR-feedback and CI-retry scans must start from the tracked
// state: listing every open issue on each 30 s cycle exhausted the token's
// hourly quota, and a rate-limited daemon cannot see a 👍 at all.
func TestPeriodicScansDoNotListOpenIssues(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())

	router.checkReactions(context.Background(), app)
	router.checkPRFeedback(context.Background(), app)
	router.retryBlockedCI(context.Background(), app)

	if got := len(calls.snapshot()); got != 0 {
		t.Fatalf("scans made %d API calls without tracked issues: %+v", got, calls.snapshot())
	}
}

// A tracked issue with nothing pending must not cost a request either.
func TestCheckReactionsSkipsIssuesWithoutTargets(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 3, func(st *IssueState) { st.Phase = "done" })
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())

	router.checkReactions(context.Background(), app)
	if got := len(calls.snapshot()); got != 0 {
		t.Fatalf("checkReactions made %d API calls for a done issue: %+v", got, calls.snapshot())
	}
}

func TestIssueNumbers(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	for _, number := range []int{9, 3, 172} {
		store.UpdateIssue("acme/app", number, func(st *IssueState) { st.Phase = "new" })
	}
	numbers := store.IssueNumbers("acme/app")
	want := []int{3, 9, 172}
	if len(numbers) != len(want) {
		t.Fatalf("IssueNumbers = %v, want %v", numbers, want)
	}
	for i := range want {
		if numbers[i] != want[i] {
			t.Fatalf("IssueNumbers = %v, want %v", numbers, want)
		}
	}
	if got := store.IssueNumbers("other/repo"); got != nil {
		t.Fatalf("IssueNumbers(unknown) = %v, want nil", got)
	}
}

// pollAPI serves the issue listing with one pull request and one plain issue,
// and answers PR fetches: an odd fetch (the poller's own read) sees the open
// PR, an even fetch (a queued review job's re-read) sees a draft and stops
// before the git work.
func pollAPI(t *testing.T, prCalls *int32) (*GH, *callLog) {
	t.Helper()
	return newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 5, "state": "open", "pull_request": map[string]any{}},
				{"number": 7, "state": "open"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/5":
			if atomic.AddInt32(prCalls, 1)%2 == 1 {
				_ = json.NewEncoder(w).Encode(autoReviewPR(5, "abc123", false, "human"))
				return
			}
			_ = json.NewEncoder(w).Encode(autoReviewPR(5, "abc123", true, "human"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/7/comments":
			_, _ = w.Write([]byte("[]"))
		default:
			_, _ = w.Write([]byte("{}"))
		}
	})
}

func TestPollOnceAutoReviewFallback(t *testing.T) {
	var prCalls int32
	gh, calls := pollAPI(t, &prCalls)
	store := newTestStore(t)
	app := &AppContext{
		Repo:       RepoConfig{FullName: "acme/app", Name: "app", AutoReview: true, Approvers: []string{"owner"}},
		GH:         gh,
		Store:      store,
		Log:        NewSilentLogger(),
		AgentLogin: "bot",
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())
	ctx := context.Background()

	router.PollOnce(ctx, "acme/app")
	router.waitInactive(ctx, "acme/app#5#review", 5*time.Second)
	if got := atomic.LoadInt32(&prCalls); got != 2 {
		t.Fatalf("PR requests = %d, want 2 (poll fetch + job fetch)", got)
	}
	if got := store.Issue("acme/app", 5).ReviewedSHA; got != "abc123" {
		t.Fatalf("ReviewedSHA = %q, want the polled head", got)
	}

	// The poller lists pull requests for the fallback but never scans their
	// comments: those still arrive through issue_comment events.
	for _, call := range calls.snapshot() {
		if call.method == http.MethodGet && call.path == "/repos/acme/app/issues/5/comments" {
			t.Fatalf("poller scanned the PR's comments: %+v", call)
		}
	}

	// The next poll sees the same head and must not queue a second review.
	router.PollOnce(ctx, "acme/app")
	if reviewJobRunning(router.Snapshot()) {
		t.Fatal("a repeated poll with the same head must not enqueue a review")
	}
	if got := atomic.LoadInt32(&prCalls); got != 3 {
		t.Fatalf("PR requests = %d, want 3 (second poll fetch only)", got)
	}
}

func TestPollOnceAutoReviewDisabled(t *testing.T) {
	var prCalls int32
	gh, _ := pollAPI(t, &prCalls)
	store := newTestStore(t)
	app := &AppContext{
		Repo:       RepoConfig{FullName: "acme/app", Name: "app", AutoReview: false, Approvers: []string{"owner"}},
		GH:         gh,
		Store:      store,
		Log:        NewSilentLogger(),
		AgentLogin: "bot",
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())

	router.PollOnce(context.Background(), "acme/app")
	if got := atomic.LoadInt32(&prCalls); got != 0 {
		t.Fatalf("disabled repo still made %d PR requests, want 0", got)
	}
	if got := store.Issue("acme/app", 5).ReviewedSHA; got != "" {
		t.Fatalf("disabled repo recorded ReviewedSHA %q", got)
	}
	if reviewJobRunning(router.Snapshot()) {
		t.Fatal("a disabled repo must not enqueue a review")
	}
}
