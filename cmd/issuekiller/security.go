package main

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// A review has two independent verdicts: what the agent thinks of the change
// and what the deterministic scan finds. The scan never sees the model's
// answer, so a prompt injection that talks the agent into "approve" cannot
// lower the risk of code that reads the bot's token or pipes a download into a
// shell.
const (
	riskNone       = "none"
	riskSuspicious = "suspicious"
	riskMalicious  = "malicious"
)

type securityFinding struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Rule   string `json:"rule"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// securityScan is both the agent's `security` block (risk, reasons) and the
// deterministic scan's result (risk, findings). mergeSecurity keeps the
// stricter of the two.
type securityScan struct {
	Risk     string            `json:"risk"`
	Reasons  []string          `json:"reasons,omitempty"`
	Findings []securityFinding `json:"findings,omitempty"`
}

// normalizeRisk maps the words a model may use onto the three levels the
// daemon acts on. An unknown non-empty answer is suspicious rather than clean:
// a malformed security block must not buy an automatic merge.
func normalizeRisk(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none", "no", "clean", "safe", "ok", "low":
		return riskNone
	case "malicious", "critical", "high", "danger", "dangerous":
		return riskMalicious
	case "suspicious", "medium", "warn", "warning", "unknown", "uncertain":
		return riskSuspicious
	}
	return riskSuspicious
}

func riskRank(risk string) int {
	switch normalizeRisk(risk) {
	case riskMalicious:
		return 3
	case riskSuspicious:
		return 2
	}
	return 1
}

func raiseRisk(current, level string) string {
	if riskRank(level) > riskRank(current) {
		return normalizeRisk(level)
	}
	return normalizeRisk(current)
}

// securityRiskBlocked reports whether the PR was marked malicious by an
// earlier review. Suspicious PRs keep the normal flow and only carry a warning.
func securityRiskBlocked(store *Store, fullName string, number int) bool {
	if store == nil {
		return false
	}
	return normalizeRisk(store.Issue(fullName, number).SecurityRisk) == riskMalicious
}

type securityLineRule struct {
	name   string
	level  string
	detail string
	re     *regexp.Regexp
}

// securityLineRules match added lines of a diff. They are deliberately broad:
// a false positive only sends the PR to a human, never the other way around.
var securityLineRules = []securityLineRule{
	{
		name: "issuekiller-state", level: riskMalicious,
		detail: "reads the bot's own state or token directory",
		re:     regexp.MustCompile(`(?i)(?:~|\$HOME|%USERPROFILE%|os\.UserHomeDir\(\))[^\n]{0,40}\.issuekiller|\.issuekiller[/\\](?:state\.json|token|config\.json|worktrees|repos)`),
	},
	{
		name: "credential-file", level: riskMalicious,
		detail: "touches a credential file",
		re:     regexp.MustCompile(`(?i)\.ssh[/\\](?:id_[a-z0-9]+|authorized_keys)|\.aws[/\\]credentials|\.netrc\b|\.docker[/\\]config\.json|\.config[/\\]gh[/\\]hosts\.yml|\.npmrc\b|\.pypirc\b`),
	},
	{
		name: "token-read", level: riskMalicious,
		detail: "reads a token or secret from the environment",
		re:     regexp.MustCompile(`(?i)(?:getenv|process\.env|os\.environ|printenv|env\[)[^\n]{0,60}(?:token|secret|password|api[_-]?key)|(?:token|secret|password|api[_-]?key)[^\n]{0,60}(?:getenv|process\.env|os\.environ|printenv)`),
	},
	{
		name: "remote-shell", level: riskMalicious,
		detail: "pipes a remote download into a shell",
		re:     regexp.MustCompile(`(?i)(?:curl|wget)[^\n|]{0,240}\|\s*(?:sudo\s+)?(?:ba|z|da|k)?sh\b|(?:ba|z)?sh\s+<\(\s*(?:curl|wget)|powershell(?:\.exe)?[^\n]{0,40}-(?:enc|encodedcommand)`),
	},
	{
		name: "encoded-exec", level: riskMalicious,
		detail: "decodes and executes an encoded payload",
		re:     regexp.MustCompile(`(?i)(?:base64\s+(?:-d|--decode|-D)[^\n]{0,120}\|\s*(?:ba|z)?sh)|(?:eval|exec)\s*\(\s*(?:atob|base64)|python[^\n]{0,40}-c[^\n]{0,120}base64`),
	},
	{
		name: "reverse-shell", level: riskMalicious,
		detail: "opens an interactive shell over the network",
		re:     regexp.MustCompile(`(?i)(?:nc|ncat|netcat)\s+[^\n]{0,80}-e\s+(?:/bin/)?(?:ba|z)?sh|/dev/tcp/`),
	},
	{
		name: "destructive-command", level: riskMalicious,
		detail: "runs a destructive command",
		re:     regexp.MustCompile(`(?i)rm\s+-[a-z]*r[a-z]*f[a-z]*\s+(?:/|~|\$HOME)(?:\s|$)|mkfs\.|dd\s+if=[^\n]{0,80}of=/dev/|:\(\)\s*\{`),
	},
	{
		name: "install-hook", level: riskMalicious,
		detail: "adds a package install hook that runs code",
		re:     regexp.MustCompile(`(?i)"(?:pre|post)(?:install|prepare)"\s*:`),
	},
	{
		name: "workflow-write-all", level: riskMalicious,
		detail: "grants the workflow write-all permissions",
		re:     regexp.MustCompile(`(?i)permissions\s*:\s*write-all`),
	},
	{
		name: "base64-payload", level: riskSuspicious,
		detail: "embeds a large encoded blob",
		re:     regexp.MustCompile(`[A-Za-z0-9+/]{300,}={0,2}`),
	},
	{
		name: "exfil-host", level: riskSuspicious,
		detail: "references a paste or tunnel host often used to exfiltrate data",
		re:     regexp.MustCompile(`(?i)webhook\.site|pastebin\.com|transfer\.sh|requestbin|ngrok\.(?:io|app)|interact\.sh|burpcollaborator|oast\.`),
	},
	{
		name: "dependency-substitution", level: riskSuspicious,
		detail: "redirects a dependency to a non-registry source",
		re:     regexp.MustCompile(`(?i)^\s*replace\s+\S+\s*=>\s*(?:git|github|https?)`),
	},
	{
		name: "prompt-injection", level: riskSuspicious,
		detail: "contains text that tries to override the agent's instructions",
		re:     regexp.MustCompile(`(?i)ignore (?:all )?(?:the )?(?:previous|above|prior) instructions|disregard (?:all )?(?:the )?(?:previous|above|prior)|system prompt`),
	},
}

// securityPathRules flag whole files. Agent instructions and configuration are
// the direct route to the bot itself, so a change there needs a human even when
// the diff looks harmless.
func securityPathFindings(file string) []securityFinding {
	clean := strings.TrimPrefix(path.Clean(file), "./")
	base := path.Base(clean)
	var findings []securityFinding
	add := func(rule, level, detail string) {
		findings = append(findings, securityFinding{File: clean, Rule: rule, Level: level, Detail: detail})
	}
	switch {
	case strings.HasPrefix(clean, "prompts/"), base == "AGENTS.md", strings.HasPrefix(clean, ".agents/"):
		add("agent-instructions", riskMalicious, "changes the agent's own instructions (prompt injection risk)")
	case strings.HasPrefix(clean, ".opencode/"), clean == "skills-lock.json":
		add("agent-config", riskMalicious, "changes the agent's own configuration")
	case clean == ".gitattributes":
		add("diff-attributes", riskSuspicious, "changes git attributes that can hide diffs")
	case strings.HasPrefix(clean, ".github/workflows/"):
		add("workflow-change", riskSuspicious, "changes CI workflows")
	}
	return findings
}

// securityContentFile skips prose: markdown cannot execute and the docs quote
// paths such as ~/.issuekiller, which would otherwise flag every documentation
// change. Prompt files are still covered by securityPathFindings.
func securityContentFile(file string) bool {
	switch strings.ToLower(path.Ext(file)) {
	case ".md", ".mdx", ".rst", ".txt", ".adoc":
		return false
	}
	return true
}

var hunkNewLine = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func hunkStart(header string) int {
	match := hunkNewLine.FindStringSubmatch(header)
	if match == nil {
		return 0
	}
	start := 0
	for _, digit := range match[1] {
		start = start*10 + int(digit-'0')
	}
	return start
}

// scanDiff walks a unified diff and inspects added lines and changed paths.
// Only added content is scanned: removing a dangerous line is not a risk.
func scanDiff(diff string) securityScan {
	scan := securityScan{Risk: riskNone}
	seenFinding := map[string]bool{}
	seenPath := map[string]bool{}
	file := ""
	pendingFile := ""
	line := 0
	inHunk := false

	add := func(finding securityFinding) {
		key := fmt.Sprintf("%s:%d:%s", finding.File, finding.Line, finding.Rule)
		if seenFinding[key] {
			return
		}
		seenFinding[key] = true
		scan.Findings = append(scan.Findings, finding)
		scan.Risk = raiseRisk(scan.Risk, finding.Level)
	}
	checkPath := func(name string) {
		if name == "" || seenPath[name] {
			return
		}
		seenPath[name] = true
		for _, finding := range securityPathFindings(name) {
			add(finding)
		}
	}

	for _, raw := range strings.Split(diff, "\n") {
		if strings.HasPrefix(raw, "diff --git ") {
			inHunk = false
			file, pendingFile = "", ""
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(raw, "--- "):
				pendingFile = strings.TrimPrefix(strings.TrimPrefix(raw, "--- "), "a/")
				continue
			case strings.HasPrefix(raw, "+++ "):
				name := strings.TrimPrefix(strings.TrimPrefix(raw, "+++ "), "b/")
				if name == "/dev/null" {
					name = pendingFile
				}
				file = name
				checkPath(file)
				continue
			}
		}
		if strings.HasPrefix(raw, "@@ ") {
			line = hunkStart(raw)
			inHunk = true
			continue
		}
		if !inHunk || raw == "" || file == "" {
			continue
		}
		switch raw[0] {
		case '+':
			content := raw[1:]
			if securityContentFile(file) {
				for _, rule := range securityLineRules {
					if rule.re.MatchString(content) {
						add(securityFinding{
							File: file, Line: line, Rule: rule.name,
							Level: rule.level, Detail: rule.detail,
						})
					}
				}
			}
			line++
		case ' ':
			line++
		}
	}
	return scan
}

// mergeSecurity keeps the stricter risk of the agent's assessment and the
// deterministic scan, and unions their reasons.
func mergeSecurity(model *securityScan, scan securityScan) securityScan {
	merged := scan
	if merged.Risk == "" {
		merged.Risk = riskNone
	}
	if model == nil {
		return merged
	}
	merged.Risk = raiseRisk(merged.Risk, model.Risk)
	for _, reason := range model.Reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		duplicate := false
		for _, have := range merged.Reasons {
			if have == reason {
				duplicate = true
				break
			}
		}
		if !duplicate {
			merged.Reasons = append(merged.Reasons, reason)
		}
	}
	return merged
}

// scanPullRequest reads the diff of the checked-out PR head and scans it. It
// runs before the coding agent so a flagged PR never executes its own code.
func scanPullRequest(ctx context.Context, app *AppContext, worktree string, number int) securityScan {
	if app.Local == nil {
		return securityScan{Risk: riskNone}
	}
	result, err := gitWorktree(ctx, worktree, app.Local.gitEnv(), 2*time.Minute, true,
		"diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"origin/"+app.Local.Base+"...HEAD")
	if err != nil || result.Code != 0 {
		app.Log.Warn("review %s#%d: security scan could not read the diff: %v", app.Repo.FullName, number, err)
		return securityScan{Risk: riskNone}
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return securityScan{Risk: riskNone}
	}
	return scanDiff(result.Stdout)
}

// staticSecurityReview is what replaces the agent when the scan matched: a
// review built only from the pattern hits, so nothing from the PR runs.
func staticSecurityReview(app *AppContext, number int, scan securityScan) reviewResult {
	key := "security.static_suspicious"
	verdict := "comment"
	if normalizeRisk(scan.Risk) == riskMalicious {
		key = "security.static_malicious"
		verdict = "request_changes"
	}
	return reviewResult{
		Verdict:  verdict,
		Summary:  app.text(number, key),
		Security: &scan,
	}
}

func formatSecurityFindings(findings []securityFinding, limit int) string {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		if len(lines) >= limit {
			break
		}
		location := finding.File
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, finding.Line)
		}
		lines = append(lines, fmt.Sprintf("- `%s` — `%s` (%s): %s",
			location, finding.Rule, finding.Level, finding.Detail))
	}
	return strings.Join(lines, "\n")
}

func formatSecurityReasons(reasons []string, limit int) string {
	lines := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if len(lines) >= limit {
			break
		}
		if reason = strings.TrimSpace(reason); reason != "" {
			lines = append(lines, "- "+strings.ReplaceAll(reason, "\n", " "))
		}
	}
	return strings.Join(lines, "\n")
}

// renderSecurityBanner is the warning at the top of the review body.
func renderSecurityBanner(app *AppContext, number int, scan securityScan) string {
	level := normalizeRisk(scan.Risk)
	key := "security.banner_suspicious"
	if level == riskMalicious {
		key = "security.banner_malicious"
	}
	return fmt.Sprintf(app.text(number, key), app.text(number, "security.level."+level))
}

func postSecurityAlert(ctx context.Context, app *AppContext, number int, scan securityScan) {
	body := []string{
		app.text(number, "security.alert.title"),
		"",
		app.text(number, "security.alert.body"),
	}
	if findings := formatSecurityFindings(scan.Findings, 10); findings != "" {
		body = append(body, "", app.text(number, "security.rules_title"), "", findings)
	}
	if reasons := formatSecurityReasons(scan.Reasons, 5); reasons != "" {
		body = append(body, "", app.text(number, "security.reasons_title"), "", reasons)
	}
	body = append(body, "", app.text(number, "security.alert.hint"))
	if _, err := app.GH.PostComment(ctx, app.Repo.FullName, number, strings.Join(body, "\n")); err != nil {
		app.Log.Warn("post security alert for %s#%d: %v", app.Repo.FullName, number, err)
	}
}

// refuseSecurityRisk answers an automatic fix or merge of a PR the review
// marked as malicious: the 👍 does not override a security block.
func refuseSecurityRisk(ctx context.Context, app *AppContext, number int) {
	if _, err := app.GH.PostComment(ctx, app.Repo.FullName, number, app.text(number, "security.blocked")); err != nil {
		app.Log.Warn("post security block for %s#%d: %v", app.Repo.FullName, number, err)
	}
}
