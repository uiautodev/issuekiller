package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newUploadApp serves GitHub's user-attachments endpoint with one distinct URL
// per upload and returns an app aimed at it.
func newUploadApp(t *testing.T, language string) (*AppContext, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, `{"url":"https://example.com/upload-%d.png"}`, calls)
	}))
	t.Cleanup(server.Close)
	gh := &GH{Token: "token", UploadBase: server.URL, Upload: server.Client()}
	app := &AppContext{
		Cfg:      &Config{Language: language},
		RepoInfo: RepoInfo{ID: 42},
		GH:       gh,
		Log:      NewSilentLogger(),
	}
	return app, &calls
}

func writeFile(t *testing.T, file string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectScreenshotsUploadsBeforeAndAfterSeparately(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "screenshots", "before-top.png"), "before")
	writeFile(t, filepath.Join(dir, "screenshots", "after-top.png"), "after")

	app, calls := newUploadApp(t, "zh")
	manifest := ResultManifest{Screenshots: []json.RawMessage{
		json.RawMessage(`{"caption":"顶部工具栏","before":"screenshots/before-top.png","after":"screenshots/after-top.png"}`),
	}}
	shots := collectScreenshots(context.Background(), app, 7, dir, manifest)

	if *calls != 2 {
		t.Fatalf("uploads = %d, want 2 (one per image)", *calls)
	}
	if len(shots) != 2 {
		t.Fatalf("shots = %d, want 2", len(shots))
	}
	if shots[0].URL == shots[1].URL {
		t.Fatalf("both shots share URL %q, want separate uploads", shots[0].URL)
	}
	if !strings.Contains(shots[0].Caption, "顶部工具栏") || !strings.Contains(shots[0].Caption, "改动前") {
		t.Fatalf("first caption = %q, want the caption plus 改动前", shots[0].Caption)
	}
	if !strings.Contains(shots[1].Caption, "顶部工具栏") || !strings.Contains(shots[1].Caption, "改动后") {
		t.Fatalf("second caption = %q, want the caption plus 改动后", shots[1].Caption)
	}
	if shots[0].Name != "before-top.png" || shots[1].Name != "after-top.png" {
		t.Fatalf("names = %q, %q, want the original files", shots[0].Name, shots[1].Name)
	}
}

func TestCollectScreenshotsEnglishCaptionsAndSingleImage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "before.png"), "before")
	writeFile(t, filepath.Join(dir, "after.png"), "after")
	writeFile(t, filepath.Join(dir, "combined.png"), "combined")

	app, _ := newUploadApp(t, "en")
	manifest := ResultManifest{Screenshots: []json.RawMessage{
		json.RawMessage(`{"caption":"Header","before":"before.png","after":"after.png"}`),
		json.RawMessage(`{"caption":"Dialog","combined":"combined.png"}`),
	}}
	shots := collectScreenshots(context.Background(), app, 7, dir, manifest)

	if len(shots) != 3 {
		t.Fatalf("shots = %d, want 3 (pair plus combined)", len(shots))
	}
	if shots[0].Caption != "Header (before)" || shots[1].Caption != "Header (after)" {
		t.Fatalf("captions = %q, %q", shots[0].Caption, shots[1].Caption)
	}
	if shots[2].Caption != "Dialog" || shots[2].Name != "combined.png" {
		t.Fatalf("combined shot = %+v, want the single image kept as is", shots[2])
	}
}

func TestCollectScreenshotsSkipsMissingPair(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "after.png"), "after")

	app, calls := newUploadApp(t, "en")
	manifest := ResultManifest{Screenshots: []json.RawMessage{
		json.RawMessage(`{"caption":"Gone","before":"missing.png","after":"after.png"}`),
	}}
	shots := collectScreenshots(context.Background(), app, 7, dir, manifest)

	if len(shots) != 0 || *calls != 0 {
		t.Fatalf("shots = %d, uploads = %d, want nothing uploaded for a broken pair", len(shots), *calls)
	}
}

func TestScreenshotsMarkdownSeparatesShots(t *testing.T) {
	md := screenshotsMarkdown([]Shot{
		{Caption: "顶部工具栏（改动前）", URL: "https://example.com/a.png"},
		{Caption: "顶部工具栏（改动后）", URL: "https://example.com/b.png"},
	})
	for _, want := range []string{"顶部工具栏（改动前）", "https://example.com/a.png", "https://example.com/b.png"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q: %s", want, md)
		}
	}
	if !strings.Contains(md, "\n\n---\n\n") {
		t.Fatalf("markdown should separate shots with a rule: %s", md)
	}
	if strings.Contains(md, "上：改动前") {
		t.Fatalf("markdown should not claim a stacked composite: %s", md)
	}
	if screenshotsMarkdown(nil) != "" {
		t.Fatal("empty shots should produce empty markdown")
	}
}
