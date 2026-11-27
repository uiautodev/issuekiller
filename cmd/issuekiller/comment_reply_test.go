package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCommentRouter builds a router whose fake API answers issue reads as
// closed, so any job HandleTrigger enqueues bails out before doing real work.
func newCommentRouter(t *testing.T, phase, blockedReason string) (*Router, *Store, *callLog) {
	t.Helper()
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = phase
		st.BlockedReason = blockedReason
	})
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/5"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "number": 5})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/5/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}, Language: "zh"},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())
	return router, store, calls
}

func commentTrigger(body, sender string) Trigger {
	return Trigger{
		Type:        "comment",
		IssueNumber: 5,
		CommentID:   7,
		CommentBody: body,
		Sender:      sender,
		Key:         "test-comment",
	}
}

// newOpenCommentRouter builds a router whose fake API reports the issue as
// open and whose Local points at a missing remote. A stage a test really
// reaches therefore fails at its first git step instead of bailing on a closed
// issue before doing anything.
func newOpenCommentRouter(t *testing.T, phase string, setup func(*IssueState)) (*Router, *Store, *callLog) {
	t.Helper()
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = phase
		if setup != nil {
			setup(st)
		}
	})
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/5"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "number": 5, "title": "add a flag", "body": "add --json",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/5/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	app := &AppContext{
		Cfg:        &Config{PlanParallel: 1, ImplementParallel: 1},
		Repo:       RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}, Language: "zh"},
		Local:      &LocalRepo{HTTPSURL: filepath.Join(t.TempDir(), "missing.git"), Base: "master", WorktreeRoot: t.TempDir()},
		GH:         gh,
		Store:      store,
		Log:        NewSilentLogger(),
		AgentLogin: "bot",
	}
	router := NewRouter(app.Cfg, map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())
	return router, store, calls
}

// postedCommentBody returns the body of a comment the router posted on the
// issue, if any.
func postedCommentBody(t *testing.T, calls *callLog) (string, bool) {
	t.Helper()
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/issues/5/comments") {
			return call.body, true
		}
	}
	return "", false
}

func postedCommentCount(calls *callLog) int {
	count := 0
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/issues/5/comments") {
			count++
		}
	}
	return count
}

func reactedWith(t *testing.T, calls *callLog, content string) bool {
	t.Helper()
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.Contains(call.path, "/reactions") &&
			strings.Contains(call.body, content) {
			return true
		}
	}
	return false
}

const failureQuote = "> ### 🤖 流程出错\n> ```\n> fetch base: git fetch failed (exit 128)\n> ```\n> \n> 回复 `@bot continue` 重试。"

// A quote-reply ending in "@bot continue" must retry the blocked task: the
// quoted failure notice is not the command, the author's own line is.
func TestQuoteReplyContinueRetriesBlocked(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger(failureQuote+"\n\n@bot continue", "owner"))
	if got := store.Issue("acme/app", 5).Phase; got != "implementing" {
		t.Fatalf("phase = %q, want implementing", got)
	}
	if !reactedWith(t, calls, "eyes") {
		t.Fatalf("no 👀 acknowledgment was posted: %+v", calls.snapshot())
	}
}

// A question on a blocked issue gets the stored failure reason quoted back,
// and is not filed as retry notes.
func TestBlockedQuestionRepliesWithReason(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 为什么失败了？", "owner"))
	if got := store.Issue("acme/app", 5).Phase; got != "blocked" {
		t.Fatalf("phase = %q, want blocked", got)
	}
	if got := store.Issue("acme/app", 5).ExtraNotes; got != "" {
		t.Fatalf("ExtraNotes = %q, want empty", got)
	}
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "fetch base: boom") || !strings.Contains(body, "continue") {
		t.Fatalf("reply = %q, want the failure reason and the retry hint", body)
	}
}

// A polite opener does not turn a real question into retry notes.
func TestBlockedPoliteQuestionRepliesWithReason(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot Could you tell me why it failed?", "owner"))
	if got := store.Issue("acme/app", 5).ExtraNotes; got != "" {
		t.Fatalf("ExtraNotes = %q, want empty", got)
	}
	body, ok := postedCommentBody(t, calls)
	if !ok || !strings.Contains(body, "fetch base: boom") {
		t.Fatalf("reply = %q, present=%v; want the failure reason", body, ok)
	}
}

// An instruction on a blocked issue is filed as ExtraNotes for the retry.
func TestBlockedInstructionSavedAsNotes(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 顺便把超时也修了", "owner"))
	if got := store.Issue("acme/app", 5).ExtraNotes; got != "顺便把超时也修了" {
		t.Fatalf("ExtraNotes = %q, want the instruction", got)
	}
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "记下了") {
		t.Fatalf("reply = %q, want the notes-saved acknowledgment", body)
	}
}

// A huge comment must not become a huge implement prompt, and an edited
// comment must not append its note a second time.
func TestBlockedNotesAreBoundedAndNotDuplicated(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	huge := strings.Repeat("x", 64000)
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot "+huge, "owner"))
	notes := store.Issue("acme/app", 5).ExtraNotes
	if len(notes) > maxExtraNotesBytes {
		t.Fatalf("ExtraNotes is %d bytes, want at most %d", len(notes), maxExtraNotesBytes)
	}
	if !strings.HasSuffix(notes, strings.Repeat("x", 100)) {
		t.Fatal("ExtraNotes kept the wrong part of the comment")
	}
	if got := postedCommentCount(calls); got != 1 {
		t.Fatalf("posted %d comments, want 1", got)
	}

	edited := commentTrigger("@bot "+huge, "owner")
	edited.Reprocess = true
	router.HandleTrigger(context.Background(), "acme/app", edited)
	if got := store.Issue("acme/app", 5).ExtraNotes; got != notes {
		t.Fatalf("edited comment changed ExtraNotes from %d to %d bytes", len(notes), len(got))
	}
	if got := postedCommentCount(calls); got != 1 {
		t.Fatalf("edited comment posted %d comments, want the original 1", got)
	}
}

func TestBlockedNoteKeepsNewestLines(t *testing.T) {
	router, store, _ := newCommentRouter(t, "blocked", "fetch base: boom")
	lines := make([]string, 0, 100)
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot "+strings.Join(lines, "\n"), "owner"))
	notes := store.Issue("acme/app", 5).ExtraNotes
	if !strings.Contains(notes, "line 100") {
		t.Fatalf("ExtraNotes = %q, want the newest line", notes)
	}
	if strings.Contains(notes, "line 1\n") {
		t.Fatalf("ExtraNotes = %q, want only the tail of a long note", notes)
	}
}

// A bare @bot is a mention, not an instruction: it must not spend a plan or
// revise run, and it gets the same acknowledgment as other no-op comments.
func TestBareMentionIsAcknowledgedNotRun(t *testing.T) {
	for _, phase := range []string{"awaiting_approval", "pr_open"} {
		t.Run(phase, func(t *testing.T) {
			router, store, calls := newCommentRouter(t, phase, "")
			router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot", "owner"))
			router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)
			if got := store.Issue("acme/app", 5).Phase; got != phase {
				t.Fatalf("phase = %q, want %q", got, phase)
			}
			if hasIssueRead(calls) {
				t.Fatal("a bare mention enqueued a stage")
			}
			body, ok := postedCommentBody(t, calls)
			if !ok || !strings.Contains(body, "收到") {
				t.Fatalf("reply = %q, present=%v; want the acknowledgment", body, ok)
			}
		})
	}
}

// "<approval word> <notes>" must actually start the implementation job. The
// phase is already "implementing" by the time the job runs, so waiting for the
// old blocked/awaiting_approval phase would bail and leave the issue stuck
// with a working label and nothing running. A broken remote makes fetchBase
// fail, which proves doImplement ran; the note stays in ExtraNotes, which the
// implement prompt renders (see TestImplementPromptIncludesOwnerNotes).
func TestApprovalNotesRetryRunsImplement(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "fetch base: boom"
		st.ExtraNotes = "之前记下的补充"
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/5"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "number": 5, "title": "add a flag", "body": "add --json",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/5/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	app := &AppContext{
		Cfg:        &Config{PlanParallel: 1, ImplementParallel: 1},
		Repo:       RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}, Language: "zh"},
		Local:      &LocalRepo{HTTPSURL: filepath.Join(t.TempDir(), "missing.git"), Base: "master", WorktreeRoot: t.TempDir()},
		GH:         gh,
		Store:      store,
		Log:        NewSilentLogger(),
		AgentLogin: "bot",
	}
	router := NewRouter(app.Cfg, map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot continue 顺便补个单测", "owner"))
	router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)

	state := store.Issue("acme/app", 5)
	if state.Phase != "blocked" || !strings.Contains(state.BlockedReason, "fetch base:") {
		t.Fatalf("the retry job did not reach doImplement (phase=%q reason=%q)", state.Phase, state.BlockedReason)
	}
	if state.ExtraNotes != "之前记下的补充\n顺便补个单测" {
		t.Fatalf("ExtraNotes = %q, want the earlier note kept and the new one appended", state.ExtraNotes)
	}
	prompt, err := renderPrompt("implement", map[string]string{
		"REPO": "acme/app", "WORKTREE": "/tmp/worktree", "BASE": "master",
		"RUN_DIR": "/tmp/run", "NUMBER": "5", "TITLE": "add a flag", "BODY": "add --json",
		"COMMENTS": "(none)", "PLAN": "(missing plan)", "NOTES": strings.TrimSpace(state.ExtraNotes),
		"BUG_INSTRUCTIONS": "", "LANGUAGE": "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "之前记下的补充\n顺便补个单测") {
		t.Fatalf("rendered implement prompt omitted approval notes: %q", prompt)
	}
}
func TestBlockedURLInstructionSavedAsNotes(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "failure")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 顺便看下 https://host/x?tab=readme 那段", "owner"))
	if got := store.Issue("acme/app", 5).ExtraNotes; got != "顺便看下 https://host/x?tab=readme 那段" {
		t.Fatalf("ExtraNotes = %q, want URL instruction", got)
	}
	if body, ok := postedCommentBody(t, calls); !ok || !strings.Contains(body, "记下了") {
		t.Fatalf("reply = %q, present=%v; want notes-saved acknowledgement", body, ok)
	}
}

// The failure reason in the question reply is bounded and an inner ``` fence
// cannot break out of the code block.
func TestBlockedQuestionBoundsReason(t *testing.T) {
	lines := make([]string, 0, 13)
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines = append(lines, "```inner fence```")
	reason := strings.Join(lines, "\n")
	router, _, calls := newCommentRouter(t, "blocked", reason)
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 为什么失败了？", "owner"))
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "line 12") {
		t.Fatalf("reply = %q, want the newest failure lines", body)
	}
	if strings.Contains(body, "line 1\n") || strings.Contains(body, "line 2\n") || strings.Contains(body, "line 3\n") {
		t.Fatalf("reply = %q, want the failure reason bounded to the last lines", body)
	}
	if strings.Contains(body, "```inner fence```") {
		t.Fatalf("reply = %q, an inner fence must be neutralised", body)
	}
	if !strings.Contains(body, "` ` `inner fence` ` `") {
		t.Fatalf("reply = %q, want the inner fence neutralised in place", body)
	}
}

// A comment while work is running is acknowledged, not silently dropped.
func TestImplementingCommentAcknowledged(t *testing.T) {
	router, store, calls := newCommentRouter(t, "implementing", "")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 先别急，还有个细节", "owner"))
	if got := store.Issue("acme/app", 5).Phase; got != "implementing" {
		t.Fatalf("phase = %q, want implementing", got)
	}
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "还在处理中") {
		t.Fatalf("reply = %q, want the still-running acknowledgment", body)
	}
}

// A non-approver @-mention gets the 👀 acknowledgment and nothing else: no
// reply, no state change, no work.
func TestNonApproverMentionOnlyGetsEyes(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot continue", "stranger"))
	if got := store.Issue("acme/app", 5).Phase; got != "blocked" {
		t.Fatalf("phase = %q, want blocked", got)
	}
	if _, ok := postedCommentBody(t, calls); ok {
		t.Fatal("a reply comment was posted for a non-approver")
	}
	if !reactedWith(t, calls, "eyes") {
		t.Fatalf("no 👀 acknowledgment was posted: %+v", calls.snapshot())
	}
}

// A comment that is only a quote has nothing new to answer: no reply text.
// (The approver still gets the 👀 "seen" reaction from the router.)
func TestQuoteOnlyCommentIgnored(t *testing.T) {
	router, store, calls := newCommentRouter(t, "blocked", "fetch base: boom")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger(failureQuote, "owner"))
	if got := store.Issue("acme/app", 5).Phase; got != "blocked" {
		t.Fatalf("phase = %q, want blocked", got)
	}
	if _, ok := postedCommentBody(t, calls); ok {
		t.Fatal("a reply comment was posted for a quote-only comment")
	}
}
func hasIssueRead(calls *callLog) bool {
	for _, call := range calls.snapshot() {
		if call.method == http.MethodGet && strings.HasSuffix(call.path, "/issues/5") {
			return true
		}
	}
	return false
}

func TestQuoteReplyProseOnAwaitingApprovalRegeneratesPlan(t *testing.T) {
	router, store, calls := newOpenCommentRouter(t, "awaiting_approval", nil)
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger(failureQuote+"\n\n改成用 flag 而不是 env", "owner"))
	router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)
	state := store.Issue("acme/app", 5)
	if state.Phase != "blocked" || !strings.Contains(state.BlockedReason, "fetch base:") {
		t.Fatalf("quoted plan feedback did not reach doPlan (phase=%q reason=%q)", state.Phase, state.BlockedReason)
	}
	if !reactedWith(t, calls, "eyes") {
		t.Fatal("quoted feedback did not receive acknowledgment")
	}
}
func TestQuoteReplyProseOnPROpenRevises(t *testing.T) {
	router, store, calls := newOpenCommentRouter(t, "pr_open", func(st *IssueState) {
		st.PRNumber = 9
		st.Branch = "agent/issue-5-test"
		st.Worktree = filepath.Join(t.TempDir(), "gone")
	})
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger(failureQuote+"\n\n改成用 flag 而不是 env", "owner"))
	router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)
	state := store.Issue("acme/app", 5)
	// doRevise validates the PR and worktree before touching the network, so
	// this reason proves the revision stage ran instead of the job bailing on
	// a closed issue.
	if state.Phase != "blocked" || !strings.Contains(state.BlockedReason, "worktree") || !strings.Contains(state.BlockedReason, "is gone") {
		t.Fatalf("quoted PR feedback did not reach doRevise (phase=%q reason=%q)", state.Phase, state.BlockedReason)
	}
	if !reactedWith(t, calls, "eyes") {
		t.Fatal("quoted feedback did not receive acknowledgment")
	}
}
func TestNonApproverMentionOnNewIssueDoesNotStartPlan(t *testing.T) {
	router, store, calls := newCommentRouter(t, "new", "")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot please help", "stranger"))
	if got := store.Issue("acme/app", 5).Phase; got != "new" {
		t.Fatalf("phase = %q, want new", got)
	}
	if !reactedWith(t, calls, "eyes") {
		t.Fatal("non-approver mention did not receive eyes")
	}
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/issues/5/comments") {
			t.Fatalf("unexpected action comment: %s", call.body)
		}
	}
	if hasIssueRead(calls) {
		t.Fatal("non-approver mention enqueued a plan")
	}
}

func TestBlockedChatterIsNotSaved(t *testing.T) {
	for _, body := range []string{"thanks", "looks good", "谢谢", "好的", "收到", "thanks, looks good", "thanks, looks good!", "thank you, looks good", "好的，谢谢"} {
		t.Run(body, func(t *testing.T) {
			router, store, calls := newCommentRouter(t, "blocked", "failure")
			router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot "+body, "owner"))
			if got := store.Issue("acme/app", 5).ExtraNotes; got != "" {
				t.Fatalf("ExtraNotes = %q, want empty", got)
			}
			reply, ok := postedCommentBody(t, calls)
			if !ok || !strings.Contains(reply, "重试") {
				t.Fatalf("reply = %q, present=%v; want the retry hint instead of silence", reply, ok)
			}
		})
	}
	t.Run("substantive content", func(t *testing.T) {
		router, store, _ := newCommentRouter(t, "blocked", "failure")
		body := "thanks, also fix the timeout"
		router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot "+body, "owner"))
		if got := store.Issue("acme/app", 5).ExtraNotes; got == "" {
			t.Fatal("ExtraNotes is empty, want substantive instruction")
		}
	})
}
func TestClosingCancelledIssueMarksClosed(t *testing.T) {
	router, store, _ := newCommentRouter(t, "cancelled", "")
	router.HandleClosed(context.Background(), "acme/app", Issue{Number: 5, State: "closed"})
	router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.Issue("acme/app", 5).IssueClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("closing a cancelled issue did not set IssueClosed")
}

func TestReopeningCancelledIssueStartsFreshPlanning(t *testing.T) {
	router, store, _ := newCommentRouter(t, "cancelled", "")
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Plan = "stale plan"
		st.IssueClosed = true
		st.PRNumber = 12
		st.PRURL = "https://github.com/acme/app/pull/12"
		st.Branch = "agent/issue-5-old"
		st.Worktree = "/tmp/gone"
		st.SessionID = "old-session"
		st.MergedAt = "2026-01-01T00:00:00Z"
		st.BlockedReason = "fetch base: boom"
	})
	router.HandleReopened(context.Background(), "acme/app", Issue{Number: 5, State: "open", User: User{Login: "owner"}})
	router.waitInactive(context.Background(), "acme/app#5", 5*time.Second)
	state := store.Issue("acme/app", 5)
	if state.Phase != "new" || state.IssueClosed || state.CancelledAt != "" {
		t.Fatalf("reopened state = %+v, want fresh new phase", state)
	}
	if state.PRNumber != 0 || state.PRURL != "" || state.Branch != "" || state.Worktree != "" ||
		state.SessionID != "" || state.MergedAt != "" || state.BlockedReason != "" {
		t.Fatalf("reopened state kept the cancelled round: %+v", state)
	}
}

// A comment on a cancelled task explains how to start over.
func TestCancelledCommentRepliesHowToRestart(t *testing.T) {
	router, store, calls := newCommentRouter(t, "cancelled", "")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 重新做一遍", "owner"))
	if got := store.Issue("acme/app", 5).Phase; got != "cancelled" {
		t.Fatalf("phase = %q, want cancelled", got)
	}
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "取消") {
		t.Fatalf("reply = %q, want the cancelled explanation", body)
	}
}

// A comment on a finished issue points at reopening instead of restarting.
func TestDoneCommentRepliesDone(t *testing.T) {
	router, _, calls := newCommentRouter(t, "done", "")
	router.HandleTrigger(context.Background(), "acme/app", commentTrigger("@bot 还有个小问题", "owner"))
	body, ok := postedCommentBody(t, calls)
	if !ok {
		t.Fatal("no reply comment was posted")
	}
	if !strings.Contains(body, "已经完成") {
		t.Fatalf("reply = %q, want the already-done reply", body)
	}
}
