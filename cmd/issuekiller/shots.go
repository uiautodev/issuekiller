package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ResultManifest struct {
	UIChanged   *bool             `json:"uiChanged"`
	Summary     string            `json:"summary"`
	Changes     []string          `json:"changes"`
	Tests       []string          `json:"tests"`
	Screenshots []json.RawMessage `json:"screenshots"`
	PRTitle     string            `json:"prTitle"`
	PRBody      string            `json:"prBody"`
	// Abort tells the harness the issue could not be reproduced; no code was
	// changed and no PR should be opened.
	Abort bool `json:"abort,omitempty"`
}

type ShotEntry struct {
	Caption  string `json:"caption"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Combined string `json:"combined"`
}

func loadManifest(runDir string) ResultManifest {
	var manifest ResultManifest
	_ = readJSONIfExists(filepath.Join(runDir, "result.json"), &manifest)
	return manifest
}

func resolveRunFile(runDir, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	base, err := filepath.Abs(runDir)
	if err != nil {
		return ""
	}
	file := value
	if !filepath.IsAbs(file) {
		file = filepath.Join(base, value)
	}
	file, err = filepath.Abs(file)
	if err != nil {
		return ""
	}
	if file != base && !strings.HasPrefix(file, base+string(os.PathSeparator)) {
		return ""
	}
	if !fileExists(file) {
		return ""
	}
	return file
}

func parseShotEntry(raw json.RawMessage) (ShotEntry, error) {
	var path string
	if err := json.Unmarshal(raw, &path); err == nil {
		return ShotEntry{After: path}, nil
	}
	var entry ShotEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ShotEntry{}, err
	}
	return entry, nil
}

// collectScreenshots uploads the images named in the manifest. A before/after
// pair stays two images: GitHub scales one tall composite down until the text
// in it is unreadable, so both are uploaded and separated by a rule in the
// comment. app is needed for the locale of the before/after captions.
func collectScreenshots(ctx context.Context, app *AppContext, number int, runDir string, manifest ResultManifest) []Shot {
	if len(manifest.Screenshots) == 0 {
		return nil
	}
	log := app.Log
	shots := make([]Shot, 0, len(manifest.Screenshots))
	appendShot := func(file, caption string) {
		if shot, ok := uploadShot(ctx, app, file, caption); ok {
			shots = append(shots, shot)
		}
	}
	for index, raw := range manifest.Screenshots {
		entry, err := parseShotEntry(raw)
		if err != nil {
			log.Warn("invalid screenshot entry: %v", err)
			continue
		}
		caption := strings.TrimSpace(entry.Caption)
		if caption == "" {
			caption = fmt.Sprintf("UI change %d", index+1)
		}

		switch {
		case entry.Combined != "":
			appendShot(resolveRunFile(runDir, entry.Combined), caption)
		case entry.Before != "" && entry.After != "":
			before := resolveRunFile(runDir, entry.Before)
			after := resolveRunFile(runDir, entry.After)
			if before == "" || after == "" {
				log.Warn("screenshot pair %q missing before/after file", caption)
				continue
			}
			appendShot(before, fmt.Sprintf(app.text(number, "common.shot_before"), caption))
			appendShot(after, fmt.Sprintf(app.text(number, "common.shot_after"), caption))
		case entry.After != "":
			log.Warn("screenshot %q has no before image, uploading after only", caption)
			appendShot(resolveRunFile(runDir, entry.After), caption)
		default:
			log.Warn("screenshot %q not found", caption)
		}
	}
	return shots
}

// uploadShot uploads one image and reports whether it became a Shot. A missing
// or empty file is warned about and dropped so one bad entry cannot cost the
// whole results section.
func uploadShot(ctx context.Context, app *AppContext, file, caption string) (Shot, bool) {
	if file == "" {
		app.Log.Warn("screenshot %q not found", caption)
		return Shot{}, false
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() == 0 {
		app.Log.Warn("screenshot %q is unreadable or empty", caption)
		return Shot{}, false
	}
	url, err := app.GH.UploadAttachment(ctx, app.RepoInfo.ID, file)
	if err != nil {
		app.Log.Warn("upload screenshot %s: %v", filepath.Base(file), err)
		return Shot{}, false
	}
	app.Log.Info("screenshot uploaded: %s -> %s", filepath.Base(file), url)
	return Shot{Name: filepath.Base(file), Caption: caption, URL: url, Size: info.Size()}, true
}

func screenshotsMarkdown(shots []Shot) string {
	if len(shots) == 0 {
		return ""
	}
	parts := make([]string, 0, len(shots))
	for _, shot := range shots {
		parts = append(parts, fmt.Sprintf("**%s**\n\n![%s](%s)", shot.Caption, shot.Caption, shot.URL))
	}
	return strings.Join(parts, "\n\n---\n\n")
}
