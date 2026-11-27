package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A pull request head may live in a fork under a different branch name, so the
// push has to take the local and remote refs separately.
func TestPushBranchToDifferentNames(t *testing.T) {
	ctx := context.Background()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	envFunc := func() ([]string, string) { return env, "" }
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if _, err := gitWorktree(ctx, dir, env, 30*time.Second, false, args...); err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
	mustGit(root, "init", "-q", "--bare", remote)
	mustGit(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(work, "add", "a.txt")
	mustGit(work, "commit", "-q", "-m", "one")

	if err := pushBranchTo(ctx, work, remote, envFunc, nil, "main", "feature-x"); err != nil {
		t.Fatalf("push local main to remote feature-x: %v", err)
	}
	if got := gitOutput(ctx, remote, env, time.Minute, "rev-parse", "--verify", "refs/heads/feature-x"); got == "" {
		t.Fatal("remote branch feature-x was not created")
	}
	if got := gitOutput(ctx, remote, env, time.Minute, "rev-parse", "--verify", "refs/heads/main"); got != "" {
		t.Fatal("push must not create the local branch name on the remote")
	}
}

// extraheaderToken extracts the bot token from the one-shot http.extraheader
// in a git environment built by buildGitEnv.
func extraheaderToken(t *testing.T, env []string) string {
	t.Helper()
	for _, entry := range env {
		rest, ok := strings.CutPrefix(entry, "GIT_CONFIG_VALUE_")
		if !ok {
			continue
		}
		if _, value, ok := strings.Cut(rest, "="); ok {
			rest = value
		}
		basic, ok := strings.CutPrefix(rest, "AUTHORIZATION: basic ")
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(basic)
		if err != nil {
			t.Fatalf("decode extraheader: %v", err)
		}
		token, ok := strings.CutPrefix(string(decoded), "x-access-token:")
		if !ok {
			t.Fatalf("extraheader = %q, want x-access-token: prefix", decoded)
		}
		return token
	}
	t.Fatal("no http.extraheader in git environment")
	return ""
}

func envEntryValue(env []string, key string) string {
	for _, entry := range env {
		if k, value, ok := strings.Cut(entry, "="); ok && k == key {
			return value
		}
	}
	return ""
}

// The bot token rotates every few hours; a git environment built once at
// startup kept authenticating with the expired token, so a fetch after a
// rotation failed with 401. gitEnv must follow the live token.
func TestLocalRepoGitEnvFollowsTokenRotation(t *testing.T) {
	token := "gho_first"
	local := &LocalRepo{token: func() string { return token }}
	if got := extraheaderToken(t, local.gitEnv()); got != "gho_first" {
		t.Fatalf("token = %q, want gho_first", got)
	}
	token = "gho_second"
	if got := extraheaderToken(t, local.gitEnv()); got != "gho_second" {
		t.Fatalf("token after rotation = %q, want gho_second", got)
	}
}

// The same rotation applies to the child-process environment: agents and
// their tools call gh with GH_TOKEN, and git with the extraheader, from
// AppContext.Env.
func TestAppContextEnvFollowsTokenRotation(t *testing.T) {
	gh := &GH{Token: "gho_first", Log: NewSilentLogger()}
	app := &AppContext{GH: gh, Identity: User{Login: "bot", ID: 1}}
	if got := extraheaderToken(t, app.Env()); got != "gho_first" {
		t.Fatalf("token = %q, want gho_first", got)
	}
	if got := envEntryValue(app.Env(), "GH_TOKEN"); got != "gho_first" {
		t.Fatalf("GH_TOKEN = %q, want gho_first", got)
	}
	gh.Token = "gho_second"
	if got := extraheaderToken(t, app.Env()); got != "gho_second" {
		t.Fatalf("token after rotation = %q, want gho_second", got)
	}
	if got := envEntryValue(app.Env(), "GH_TOKEN"); got != "gho_second" {
		t.Fatalf("GH_TOKEN after rotation = %q, want gho_second", got)
	}
	if got := envEntryValue(app.Env(), "GITHUB_TOKEN"); got != "gho_second" {
		t.Fatalf("GITHUB_TOKEN after rotation = %q, want gho_second", got)
	}
}

// gitWithAuthRetry exists because git can be the first consumer to meet an
// expired token: a webhook job right at expiry would otherwise fail before the
// API poll notices the 401. It must rotate once and retry with a rebuilt env.
func TestGitWithAuthRetryFollowsRotation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	token := "gho_first"
	var seen []string
	env := func() ([]string, string) {
		seen = append(seen, token)
		return buildGitEnv(nil, token, User{}), token
	}
	rotations := 0
	rotate := func(_ context.Context, failed string) bool {
		rotations++
		if failed != "gho_first" {
			t.Fatalf("failed token = %q, want gho_first", failed)
		}
		token = "gho_second"
		return true
	}
	_, err := gitWithAuthRetry(context.Background(), t.TempDir(), env, rotate, 30*time.Second, false,
		"ls-remote", server.URL+"/repo.git")
	if err == nil {
		t.Fatal("unauthenticated remote should still fail after one retry")
	}
	if rotations != 1 {
		t.Fatalf("rotations = %d, want 1", rotations)
	}
	if len(seen) != 2 || seen[0] != "gho_first" || seen[1] != "gho_second" {
		t.Fatalf("environments built for %v, want [gho_first gho_second]", seen)
	}
}

func TestGitWithAuthRetryUsesRotatedTokenAfterRefreshError(t *testing.T) {
	bin := t.TempDir()
	gitPath := filepath.Join(bin, "git")
	script := "#!/bin/sh\nif [ \"$TOKEN\" = gho_first ]; then\n  echo 'fatal: Authentication failed' >&2\n  exit 128\nfi\nexit 0\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TOKEN", "gho_first")

	token := "gho_first"
	local := &LocalRepo{
		token: func() string { return token },
		refresh: func(_ context.Context, failed string) (string, error) {
			if failed != "gho_first" {
				t.Fatalf("failed token = %q, want gho_first", failed)
			}
			token = "gho_second"
			t.Setenv("TOKEN", token)
			return "", fmt.Errorf("another refresh already rotated token")
		},
	}
	result, err := gitWithAuthRetry(context.Background(), t.TempDir(), local.gitEnvWithToken, local.refreshToken, 30*time.Second, false, "status")
	if err != nil || result.Code != 0 || token != "gho_second" {
		t.Fatalf("retry result = %+v, err = %v, token = %q; want success with gho_second", result, err, token)
	}
}

// An ordinary git failure must not rotate the token or run the command twice.
func TestGitWithAuthRetryIgnoresOtherFailures(t *testing.T) {
	rotations := 0
	envCalls := 0
	env := func() ([]string, string) {
		envCalls++
		return os.Environ(), ""
	}
	rotate := func(context.Context, string) bool {
		rotations++
		return true
	}
	_, err := gitWithAuthRetry(context.Background(), t.TempDir(), env, rotate, 30*time.Second, false,
		"rev-parse", "--verify", "refs/heads/missing")
	if err == nil {
		t.Fatal("missing ref should fail")
	}
	if rotations != 0 || envCalls != 1 {
		t.Fatalf("rotations = %d envCalls = %d, want 0 and 1", rotations, envCalls)
	}
}

// A git-first 401 rotates through the same GH refresh the API path uses.
func TestAppContextRefreshTokenRotates(t *testing.T) {
	refreshes := 0
	gh := &GH{Token: "gho_first", Log: NewSilentLogger(), refresh: func(context.Context) (string, error) {
		refreshes++
		return "gho_second", nil
	}}
	app := &AppContext{GH: gh, Log: NewSilentLogger()}
	if !app.refreshToken(context.Background(), "gho_first") {
		t.Fatal("refreshToken = false, want true after a rotation")
	}
	if got := gh.currentToken(); got != "gho_second" {
		t.Fatalf("token = %q, want gho_second", got)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if (&AppContext{}).refreshToken(context.Background(), "gho_first") {
		t.Fatal("a context without GH cannot refresh")
	}
}

func TestAppContextRefreshTokenReusesConcurrentRotation(t *testing.T) {
	refreshes := 0
	gh := &GH{Token: "gho_first", Log: NewSilentLogger(), refresh: func(context.Context) (string, error) {
		refreshes++
		return "gho_second", nil
	}}
	app := &AppContext{GH: gh, Log: NewSilentLogger()}
	for range 2 {
		if !app.refreshToken(context.Background(), "gho_first") {
			t.Fatal("refreshToken = false, want existing rotated token")
		}
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 for concurrent failures using the same token", refreshes)
	}
	if got := gh.currentToken(); got != "gho_second" {
		t.Fatalf("token = %q, want gho_second", got)
	}
}

// LocalRepo hands the failed token to the GH refresh hook and only reports a
// retry when the value actually changed.
func TestLocalRepoRefreshTokenUsesHook(t *testing.T) {
	token := "gho_first"
	var failed string
	local := &LocalRepo{
		token: func() string { return token },
		refresh: func(_ context.Context, stale string) (string, error) {
			failed = stale
			token = "gho_second"
			return token, nil
		},
	}
	if !local.refreshToken(context.Background(), "gho_first") {
		t.Fatal("refreshToken = false, want true")
	}
	if failed != "gho_first" {
		t.Fatalf("refresh failed token = %q, want gho_first", failed)
	}
	if token != "gho_second" {
		t.Fatalf("token = %q, want gho_second", token)
	}
	if local.refreshToken(context.Background(), "gho_second") {
		t.Fatal("refreshToken = true when the value did not change")
	}
}

// The initial clone can run for 15 minutes, so it must not pin the token the
// daemon verified at startup: a 401 there uses the same refresh and retry.
func TestResolveRepoCloneRetriesAfterTokenRotation(t *testing.T) {
	stateDirOverride = t.TempDir()
	t.Cleanup(func() { stateDirOverride = "" })
	bin := t.TempDir()
	counter := filepath.Join(t.TempDir(), "clone-count")
	script := "#!/bin/sh\n" +
		"n=0\n" +
		"[ -f \"$COUNT_FILE\" ] && n=$(cat \"$COUNT_FILE\")\n" +
		"n=$((n+1))\n" +
		"echo $n > \"$COUNT_FILE\"\n" +
		"if [ \"$n\" = 1 ]; then\n" +
		"  echo 'fatal: Authentication failed' >&2\n" +
		"  exit 128\n" +
		"fi\n" +
		"mkdir -p \"$3/.git\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COUNT_FILE", counter)

	token := "gho_first"
	var failed string
	refresh := func(_ context.Context, stale string) (string, error) {
		failed = stale
		token = "gho_second"
		return token, nil
	}
	local, err := resolveRepo(context.Background(),
		RepoConfig{Owner: "acme", Name: "app", FullName: "acme/app"},
		RepoInfo{DefaultBranch: "main"}, User{},
		func() string { return token }, refresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	if failed != "gho_first" {
		t.Fatalf("refresh failed token = %q, want gho_first", failed)
	}
	if got := local.token(); got != "gho_second" {
		t.Fatalf("repo token = %q, want gho_second", got)
	}
}

// The live failure from the bug report and GitHub's 401 wording must both be
// recognised; a plain missing ref must not be mistaken for an auth failure.
func TestGitAuthFailedMarkers(t *testing.T) {
	for _, stderr := range []string{
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"fatal: unable to access 'https://github.com/acme/app.git/': The requested URL returned error: 401",
		"remote: Invalid username or password.",
		"fatal: Authentication failed for 'https://github.com/acme/app.git/'",
	} {
		if !gitAuthFailed(CmdResult{Code: 128, Stderr: stderr}) {
			t.Fatalf("gitAuthFailed(%q) = false, want true", stderr)
		}
	}
	if gitAuthFailed(CmdResult{Code: 128, Stderr: "fatal: couldn't find remote ref refs/pull/1/head"}) {
		t.Fatal("a missing ref must not trigger a token refresh")
	}
}

// The review flow can push to a PR branch between two issue revisions; the
// revision must build on that head instead of failing its own push.
func TestSyncWorktreeBranchFastForwardsOrReportsDivergence(t *testing.T) {
	ctx := context.Background()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	envFunc := func() ([]string, string) { return env, "" }
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if _, err := gitWorktree(ctx, dir, env, 30*time.Second, false, args...); err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
	writeFile := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(root, "init", "-q", "--bare", remote)
	mustGit(root, "init", "-q", "-b", "main", work)
	writeFile(work, "a.txt", "one\n")
	mustGit(work, "add", "a.txt")
	mustGit(work, "commit", "-q", "-m", "one")
	mustGit(work, "checkout", "-q", "-b", "feature")
	if err := pushBranchTo(ctx, work, remote, envFunc, nil, "feature", "feature"); err != nil {
		t.Fatalf("seed remote branch: %v", err)
	}

	// Another writer (the review fix) advances the branch.
	other := filepath.Join(root, "other")
	mustGit(root, "clone", "-q", remote, other)
	mustGit(other, "checkout", "-q", "feature")
	writeFile(other, "b.txt", "two\n")
	mustGit(other, "add", "b.txt")
	mustGit(other, "commit", "-q", "-m", "two")
	mustGit(other, "push", "-q", "origin", "feature")

	if err := syncWorktreeBranch(ctx, work, remote, envFunc, nil, "feature"); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	want := gitOutput(ctx, other, env, time.Minute, "rev-parse", "HEAD")
	if got := gitOutput(ctx, work, env, time.Minute, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree head = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); err != nil {
		t.Fatal("fast-forward did not bring the pushed file into the worktree")
	}

	// A local commit plus a different remote commit cannot fast-forward.
	writeFile(work, "c.txt", "local\n")
	mustGit(work, "add", "c.txt")
	mustGit(work, "commit", "-q", "-m", "local")
	writeFile(other, "d.txt", "remote\n")
	mustGit(other, "add", "d.txt")
	mustGit(other, "commit", "-q", "-m", "remote")
	mustGit(other, "push", "-q", "origin", "feature")
	if err := syncWorktreeBranch(ctx, work, remote, envFunc, nil, "feature"); err == nil {
		t.Fatal("a diverged branch must be reported instead of merged or force-pushed")
	}
}
