package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChecksAllGreen(t *testing.T) {
	ok := conclusion("success")
	skip := conclusion("skipped")
	fail := conclusion("failure")
	now := time.Now()

	cases := []struct {
		name string
		runs CheckRuns
		want bool
	}{
		{"empty", CheckRuns{}, false},
		{"all green", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "completed", ok, now, &now),
			testCheckRun("b", "completed", skip, now, &now),
		}}, true},
		{"one failure", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "completed", ok, now, &now),
			testCheckRun("b", "completed", fail, now, &now),
		}}, false},
		{"still running", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "in_progress", nil, now, &now),
		}}, false},
	}
	for _, tc := range cases {
		if got := checksAllGreen(tc.runs); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A task parked by a failed CI check remains eligible until its merge job
// actually runs, so a dropped job can be recovered by a later scan.
func TestRetryBlockedCIRetriesAfterDroppedJob(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "test: failure"
		st.BlockedByCI = true
		st.PRNumber = 50
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 5, "state": "open"}})
		case r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
				"head": map[string]any{"sha": "abc123"},
				"base": map[string]any{"ref": "master"},
			})
		case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"name": "test", "status": "completed", "conclusion": "success"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	// A missing pool drops enqueue, as can a process exit before its queued job
	// starts. The persisted flag must survive for a newly constructed router.
	first := &Router{
		store: store, log: NewSilentLogger(), pools: map[string]*repoPool{},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	first.retryBlockedCI(context.Background(), app)
	if got := store.Issue("acme/app", 5).BlockedByCI; !got {
		t.Fatal("BlockedByCI was cleared before the merge job ran")
	}

	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	second := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	second.retryBlockedCI(context.Background(), app)
	if len(pool.work) != 1 {
		t.Fatalf("next router scan queued %d jobs, want 1", len(pool.work))
	}
	if got := store.Issue("acme/app", 5).BlockedByCI; !got {
		t.Fatal("BlockedByCI was cleared when retry was only queued")
	}
}

// A duplicated scan must not repeat the "retrying the merge" line while the
// job is only queued: that log line is the signal used to spot a lost retry.
func TestRetryBlockedCILogsOnlyWhenQueued(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "test: failure"
		st.BlockedByCI = true
		st.PRNumber = 50
	})
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
				"head": map[string]any{"sha": "abc123"},
				"base": map[string]any{"ref": "master"},
			})
		case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
			_ = json.NewEncoder(w).Encode(CheckRuns{TotalCount: 1, CheckRuns: []CheckRun{
				testCheckRun("test", "completed", conclusion("success"), time.Now(), nil),
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	pool := &repoPool{plan: make(chan routerJob, 2), work: make(chan routerJob, 2), ci: make(chan routerJob, 2)}
	var logs bytes.Buffer
	router := &Router{
		store: store, log: &Logger{level: levelInfo, out: &logs},
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	router.retryBlockedCI(context.Background(), app)
	firstCalls := calls.snapshot()
	router.retryBlockedCI(context.Background(), app)
	secondCalls := calls.snapshot()
	for _, call := range secondCalls[len(firstCalls):] {
		if call.path == "/repos/acme/app/pulls/50" || strings.HasSuffix(call.path, "/check-runs") {
			t.Fatalf("second scan made unnecessary API request: %+v", call)
		}
	}
	if got := len(pool.work); got != 1 {
		t.Fatalf("scan queued %d jobs, want 1", got)
	}
	if got := strings.Count(logs.String(), "retrying the merge"); got != 1 {
		t.Fatalf("retry log lines = %d, want 1 while the job is queued", got)
	}
}

// A merge interrupted after the phase moved to merging is resumed regardless
// of BlockedByCI, but only while no live or queued job owns the issue.
func TestRetryBlockedCIResumesInterruptedMerge(t *testing.T) {
	for _, blockedByCI := range []bool{true, false} {
		for _, owned := range []string{"", "active", "queued"} {
			name := "no job"
			if owned != "" {
				name = owned + " job"
			}
			if !blockedByCI {
				name = "plain merge, " + name
			}
			t.Run(name, func(t *testing.T) {
				store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
				store.UpdateIssue("acme/app", 5, func(st *IssueState) {
					st.Phase = "merging"
					st.BlockedByCI = blockedByCI
					st.PRNumber = 50
				})
				pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
				router := &Router{
					store: store, log: NewSilentLogger(),
					pools:  map[string]*repoPool{"acme/app": pool},
					active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
				}
				key := "acme/app#5"
				if owned == "active" {
					router.active[key] = true
				}
				if owned == "queued" {
					router.queued[key] = true
				}
				app := &AppContext{Repo: RepoConfig{FullName: "acme/app"}, Store: store, Log: NewSilentLogger()}
				router.retryBlockedCI(context.Background(), app)
				want := 0
				if owned == "" {
					want = 1
				}
				if got := len(pool.work); got != want {
					t.Fatalf("scan queued %d jobs, want %d", got, want)
				}
			})
		}
	}
}

// A resume that exits through a guard before doMerge must not consume the
// process's attempt: a transient GetIssue failure leaves the issue at merging
// and the next scan must queue the resume again.
func TestRetryBlockedCIGuardFailureStaysRetryable(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "merging"
		st.PRNumber = 50
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app"},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	router := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	key := "acme/app#5"
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("first scan queued %d jobs, want 1", got)
	}
	job := <-pool.work
	// Run the job the way the worker would, then release the key: the GetIssue
	// failure leaves the phase at merging.
	router.mu.Lock()
	delete(router.queued, key)
	router.active[key] = true
	router.mu.Unlock()
	job.fn(context.Background())
	router.mu.Lock()
	delete(router.active, key)
	router.mu.Unlock()
	if got := store.Issue("acme/app", 5).Phase; got != "merging" {
		t.Fatalf("phase = %q, want merging after the failed resume", got)
	}
	if router.resumeAlreadyTried(key, 50) {
		t.Fatal("guard exit consumed the resume attempt")
	}
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("next scan queued %d jobs, want 1", got)
	}
}

// Once a resume actually enters doMerge, a crash that leaves phase=merging
// must not restart every poll. The process remembers one attempt per PR; a
// new process (a restart) gets one fresh attempt.
func TestRetryBlockedCIResumeIsBoundedPerProcess(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "merging"
		st.PRNumber = 50
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "open"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
				"base": map[string]any{"ref": "master"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/app/pulls/50/merge":
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	app := &AppContext{
		Cfg:   &Config{MaxFixRounds: 1},
		Repo:  RepoConfig{FullName: "acme/app"},
		Local: &LocalRepo{Base: "master"},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	router := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	key := "acme/app#5"
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("first scan queued %d jobs, want 1", got)
	}
	job := <-pool.work
	router.mu.Lock()
	delete(router.queued, key)
	router.active[key] = true
	router.mu.Unlock()
	job.fn(context.Background())
	router.mu.Lock()
	delete(router.active, key)
	router.mu.Unlock()
	if got := store.Issue("acme/app", 5).Phase; got != "done" {
		t.Fatalf("phase = %q, want done after the resumed merge", got)
	}
	if !router.resumeAlreadyTried(key, 50) {
		t.Fatal("entering doMerge did not consume the resume attempt")
	}
	// Simulate that merge crashing after it left phase=merging behind.
	store.UpdateIssue("acme/app", 5, func(st *IssueState) { st.Phase = "merging" })
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 0 {
		t.Fatalf("second scan in the same process queued %d jobs, want 0", got)
	}

	restartPool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	restarted := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": restartPool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	restarted.retryBlockedCI(context.Background(), app)
	if got := len(restartPool.work); got != 1 {
		t.Fatalf("restarted process queued %d jobs, want 1", got)
	}
}

// The one-shot resume is per PR: a reopened issue that gets a new PR must not
// inherit the previous round's mark.
func TestRetryBlockedCIResumeIsPerPR(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "merging"
		st.PRNumber = 50
	})
	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	router := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	app := &AppContext{Repo: RepoConfig{FullName: "acme/app"}, Store: store, Log: NewSilentLogger()}
	key := "acme/app#5"
	router.markResumed(key, 50)
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 0 {
		t.Fatalf("same PR queued %d jobs, want 0", got)
	}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) { st.PRNumber = 51 })
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("new PR queued %d jobs, want 1", got)
	}
}

// Re-entering doMerge for an interrupted merge finishes the issue when the PR
// already merged: close it, record the merge and remove the worktree. GitHub
// closes the issue through the PR body's "Closes #N", so the production crash
// leaves it closed, not open.
func TestRetryBlockedCICompletesAlreadyMergedPR(t *testing.T) {
	for _, issueState := range []string{"open", "closed"} {
		t.Run(issueState+" issue", func(t *testing.T) {
			oldState := stateDirOverride
			stateDirOverride = t.TempDir()
			t.Cleanup(func() { stateDirOverride = oldState })
			worktree := filepath.Join(treesDir(), "issue-5")
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				t.Fatalf("create worktree: %v", err)
			}
			store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
			store.UpdateIssue("acme/app", 5, func(st *IssueState) {
				st.Phase = "merging"
				st.BlockedByCI = true
				st.PRNumber = 50
				st.Branch = "agent/issue-5-test"
				st.Worktree = worktree
			})
			gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": issueState})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/50":
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 50, "state": "closed", "merged": true})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
					_ = json.NewEncoder(w).Encode([]any{})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{})
				}
			})
			app := &AppContext{
				Cfg:   &Config{MaxFixRounds: 1},
				Repo:  RepoConfig{FullName: "acme/app"},
				Local: &LocalRepo{Dir: t.TempDir()},
				GH:    gh,
				Store: store,
				Log:   NewSilentLogger(),
			}
			pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
			router := &Router{
				store: store, log: NewSilentLogger(),
				pools:  map[string]*repoPool{"acme/app": pool},
				active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
			}
			router.retryBlockedCI(context.Background(), app)
			if got := len(pool.work); got != 1 {
				t.Fatalf("scan queued %d jobs, want 1", got)
			}
			job := <-pool.work
			job.fn(context.Background())
			got := store.Issue("acme/app", 5)
			if got.Phase != "done" {
				t.Fatalf("phase = %q, want done", got.Phase)
			}
			if got.BlockedByCI {
				t.Fatal("finishing an already-merged PR kept BlockedByCI set")
			}
			if got.MergedAt == "" {
				t.Fatal("finishing an already-merged PR did not record MergedAt")
			}
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatalf("worktree %s still exists after the recovered merge", worktree)
			}
			closed, commented := false, false
			for _, call := range calls.snapshot() {
				if call.method == http.MethodPatch && call.path == "/repos/acme/app/issues/5" &&
					strings.Contains(call.body, `"state":"closed"`) {
					closed = true
				}
				if call.method == http.MethodPost && call.path == "/repos/acme/app/issues/5/comments" {
					commented = true
				}
			}
			if wantClosed := issueState == "open"; closed != wantClosed {
				t.Fatalf("close call = %v, want %v for a %s issue", closed, wantClosed, issueState)
			}
			if !commented {
				t.Fatal("finishing an already-merged PR did not post the merge-done comment")
			}
		})
	}
}

// A closed issue whose PR did not merge is a cancelled task, not a merge to
// finish. Leaving phase=merging would make every poll enqueue the same job.
func TestRetryBlockedCICancelsClosedIssueWithUnmergedPR(t *testing.T) {
	oldState := stateDirOverride
	stateDirOverride = t.TempDir()
	t.Cleanup(func() { stateDirOverride = oldState })
	worktree := filepath.Join(treesDir(), "issue-5")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "merging"
		st.BlockedByCI = true
		st.PRNumber = 50
		st.Branch = "agent/issue-5-test"
		st.Worktree = worktree
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "closed"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 50, "state": "closed", "merged": false})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	app := &AppContext{
		Cfg:   &Config{MaxFixRounds: 1},
		Repo:  RepoConfig{FullName: "acme/app"},
		Local: &LocalRepo{Dir: t.TempDir()},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	router := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("scan queued %d jobs, want 1", got)
	}
	job := <-pool.work
	job.fn(context.Background())
	got := store.Issue("acme/app", 5)
	if got.Phase != "cancelled" {
		t.Fatalf("phase = %q, want cancelled", got.Phase)
	}
	if got.BlockedByCI {
		t.Fatal("cancelling a closed issue kept BlockedByCI set")
	}
	if got.CancelledAt == "" {
		t.Fatal("cancelling a closed issue did not record CancelledAt")
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after the cancelled merge", worktree)
	}
}

// Guard exits before doMerge keep the retry flag so a later scan can try again.
// A closed issue is terminal: it must cancel and clear the flag instead of
// re-arming the same warn-only job every poll.
// A PR closed without merging while the issue is still open must end the task
// and clean up, not mark the open issue done: that would look like a reopen on
// the next poll and start a new planning round.
func TestRetryBlockedCICancelsOpenIssueWithClosedPR(t *testing.T) {
	oldState := stateDirOverride
	stateDirOverride = t.TempDir()
	t.Cleanup(func() { stateDirOverride = oldState })
	worktree := filepath.Join(treesDir(), "issue-5")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "merging"
		st.BlockedByCI = true
		st.PRNumber = 50
		st.Branch = "agent/issue-5-test"
		st.Worktree = worktree
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "open"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 50, "state": "closed", "merged": false})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	app := &AppContext{
		Cfg:   &Config{MaxFixRounds: 1},
		Repo:  RepoConfig{FullName: "acme/app"},
		Local: &LocalRepo{Dir: t.TempDir()},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	router := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	router.retryBlockedCI(context.Background(), app)
	if got := len(pool.work); got != 1 {
		t.Fatalf("scan queued %d jobs, want 1", got)
	}
	job := <-pool.work
	job.fn(context.Background())
	got := store.Issue("acme/app", 5)
	if got.Phase != "cancelled" {
		t.Fatalf("phase = %q, want cancelled", got.Phase)
	}
	if got.IssueClosed {
		t.Fatal("cancelling an open issue recorded it as closed")
	}
	if got.BlockedByCI {
		t.Fatal("cancelling a closed PR kept BlockedByCI set")
	}
	if wasReopened(got) {
		t.Fatal("a cancelled open issue must not look reopened to the poller")
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after the cancelled merge", worktree)
	}
}

func TestRetryBlockedCIJobGuards(t *testing.T) {
	cases := []struct {
		name       string
		issueCode  int
		issueState string
		afterQueue func(*Store)
		wantPhase  string
		wantFlag   bool
	}{
		{"get issue error", http.StatusInternalServerError, "open", nil, "blocked", true},
		{"issue closed", 0, "closed", nil, "cancelled", false},
		{"phase changed", 0, "open", func(store *Store) {
			store.UpdateIssue("acme/app", 5, func(st *IssueState) { st.Phase = "implementing" })
		}, "implementing", true},
		{"no PR", 0, "open", func(store *Store) {
			store.UpdateIssue("acme/app", 5, func(st *IssueState) { st.PRNumber = 0 })
		}, "blocked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
			store.UpdateIssue("acme/app", 5, func(st *IssueState) {
				st.Phase = "blocked"
				st.BlockedReason = "test: failure"
				st.BlockedByCI = true
				st.PRNumber = 50
			})
			gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
					if tc.issueCode != 0 {
						w.WriteHeader(tc.issueCode)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": tc.issueState})
				case r.URL.Path == "/repos/acme/app/pulls/50":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
						"head": map[string]any{"sha": "abc123"},
						"base": map[string]any{"ref": "master"},
					})
				case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
					_ = json.NewEncoder(w).Encode(CheckRuns{TotalCount: 1, CheckRuns: []CheckRun{
						testCheckRun("test", "completed", conclusion("success"), time.Now(), nil),
					}})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			app := &AppContext{
				Repo:  RepoConfig{FullName: "acme/app"},
				GH:    gh,
				Store: store,
				Log:   NewSilentLogger(),
			}
			pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
			router := &Router{
				store: store, log: NewSilentLogger(),
				pools:  map[string]*repoPool{"acme/app": pool},
				active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
			}
			router.retryBlockedCI(context.Background(), app)
			if got := len(pool.work); got != 1 {
				t.Fatalf("scan queued %d jobs, want 1", got)
			}
			if tc.afterQueue != nil {
				tc.afterQueue(store)
			}
			job := <-pool.work
			job.fn(context.Background())
			got := store.Issue("acme/app", 5)
			if got.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.BlockedByCI != tc.wantFlag {
				t.Fatalf("BlockedByCI = %v, want %v", got.BlockedByCI, tc.wantFlag)
			}
		})
	}
}

// The retry job only consumes the flag on a non-CI terminal outcome; a CI
// re-block must stay retryable.
func TestRetryBlockedCIMergeJobFlagRule(t *testing.T) {
	cases := []struct {
		name        string
		ciGreen     bool
		mergeResult map[string]any
		wantPhase   string
		wantFlag    bool
	}{
		{"merge failure clears", true, map[string]any{"merged": false, "message": "merge rejected"}, "blocked", false},
		{"CI re-block keeps", false, nil, "blocked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
			store.UpdateIssue("acme/app", 5, func(st *IssueState) {
				st.Phase = "blocked"
				st.BlockedReason = "test: failure"
				st.BlockedByCI = true
				st.PRNumber = 50
			})
			green := true
			gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/issues/5":
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "open"})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/50":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
						"head": map[string]any{"sha": "abc123"},
						"base": map[string]any{"ref": "master"},
					})
				case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs") ||
					strings.HasSuffix(r.URL.Path, "/commits/master/check-runs"):
					result := "success"
					if !green {
						result = "failure"
					}
					_ = json.NewEncoder(w).Encode(CheckRuns{TotalCount: 1, CheckRuns: []CheckRun{
						testCheckRun("test", "completed", conclusion(result), time.Now(), nil),
					}})
				case strings.Contains(r.URL.Path, "/protection/required_status_checks"):
					w.WriteHeader(http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
					_ = json.NewEncoder(w).Encode([]any{})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/app/pulls/50/merge":
					_ = json.NewEncoder(w).Encode(tc.mergeResult)
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{})
				}
			})
			app := &AppContext{
				Cfg:   &Config{MaxFixRounds: 1},
				Repo:  RepoConfig{FullName: "acme/app"},
				GH:    gh,
				Store: store,
				Log:   NewSilentLogger(),
			}
			pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
			router := &Router{
				store: store, log: NewSilentLogger(),
				pools:  map[string]*repoPool{"acme/app": pool},
				active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
			}
			router.retryBlockedCI(context.Background(), app)
			if got := len(pool.work); got != 1 {
				t.Fatalf("scan queued %d jobs, want 1", got)
			}
			green = tc.ciGreen
			job := <-pool.work
			job.fn(context.Background())
			got := store.Issue("acme/app", 5)
			if got.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.BlockedByCI != tc.wantFlag {
				t.Fatalf("BlockedByCI = %v, want %v", got.BlockedByCI, tc.wantFlag)
			}
		})
	}
}

// An action dropped because another job owns the key must not move the phase,
// or the owning job exits and leaves a phase with no job behind it.
func TestDroppedReactionDoesNotMovePhase(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		run   func(*Router, *AppContext)
	}{
		{"approve", "blocked", func(router *Router, app *AppContext) {
			router.enqueueApprove(context.Background(), app, "acme/app", 5, "owner")
		}},
		{"merge", "pr_open", func(router *Router, app *AppContext) {
			router.enqueueMerge(app, "acme/app", 5, "owner")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
			store.UpdateIssue("acme/app", 5, func(st *IssueState) {
				st.Phase = tc.phase
				st.BlockedByCI = true
				st.PRNumber = 50
			})
			pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
			router := &Router{
				store: store, log: NewSilentLogger(),
				pools:  map[string]*repoPool{"acme/app": pool},
				active: map[string]bool{}, queued: map[string]bool{"acme/app#5": true}, cancels: map[string]context.CancelFunc{},
			}
			app := &AppContext{Repo: RepoConfig{FullName: "acme/app"}, Store: store, Log: NewSilentLogger()}
			tc.run(router, app)
			if got := store.Issue("acme/app", 5).Phase; got != tc.phase {
				t.Fatalf("phase = %q, want %q unchanged", got, tc.phase)
			}
			if got := len(pool.work); got != 0 {
				t.Fatalf("dropped action queued %d jobs, want 0", got)
			}
		})
	}
}

// A blocked task whose reason is not CI (for example local checks) is left for
// the approver even when the PR is green.
func TestRetryBlockedCISkipsOtherReasons(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 6, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "checks failed"
		st.PRNumber = 60
	})
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 6, "state": "open"}})
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app"},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())

	router.retryBlockedCI(context.Background(), app)
	for _, call := range calls.snapshot() {
		if strings.Contains(call.path, "/pulls/") || strings.Contains(call.path, "/check-runs") {
			t.Fatalf("unexpected call for a non-CI block: %+v", call)
		}
	}
}
