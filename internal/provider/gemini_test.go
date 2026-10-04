package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	coraassets "github.com/herikwebb/cora"
	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func geminiTestAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", dir)
	if err := os.Mkdir(filepath.Join(dir, ".gemini"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gemini", "oauth_creds.json"), []byte(`{"refresh_token":"fixture-refresh-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func geminiTestRequest(t *testing.T) Request {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/app.go b/app.go\n+package app\n")
	digest := sha256.Sum256(patch)
	return Request{
		WorkDir: root, RepoRoot: root, RuntimeDir: t.TempDir(), RunDir: t.TempDir(),
		Timeout: time.Second, Schema: coraassets.ReviewSchema, SnapshotPatch: patch,
		Prompt: "Review the exact change.", Policy: "Trusted CORA policy.",
		Target:       model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: hex.EncodeToString(digest[:])},
		ChangedPaths: []string{"app.go"},
	}
}

func geminiTestCLI(t *testing.T, version, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(t.TempDir(), "gemini")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  printf '%s\\n' '" + version + "'\n  exit 0\nfi\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func geminiSuccessEnvelope() string {
	return `{"response":"{\"schema_version\":\"1\",\"verdict\":\"approve\",\"context_complete\":true,\"summary\":\"Reviewed.\",\"findings\":[],\"reviewed_paths\":[\"app.go\"],\"omitted_paths\":[],\"residual_risks\":[]}","stats":{"models":{"gemini-2.5-pro":{"api":{"totalRequests":2},"tokens":{"prompt":100,"cached":20,"candidates":30,"thoughts":40}}}}}`
}

func TestGeminiReviewCompletesWithPrivateConfiguration(t *testing.T) {
	geminiTestAuth(t)
	request := geminiTestRequest(t)
	capture := filepath.Join(t.TempDir(), "session-path")
	command := geminiTestCLI(t, "0.46.0", `
test "$GEMINI_CLI_HOME" = "$HOME" || exit 70
test -f "$GEMINI_CLI_HOME/.gemini/oauth_creds.json" || exit 71
test -f source/app.go || exit 72
test -f target.diff || exit 73
test -f "$GEMINI_SYSTEM_MD" || exit 74
printf '%s' "$GEMINI_CLI_HOME" > '`+capture+`'
cat >/dev/null
printf '%s\n' '`+geminiSuccessEnvelope()+`'
`)
	cfg := config.Defaults().Reviewers.Gemini
	cfg.Command = command
	result := (Gemini{Config: cfg}).Review(context.Background(), request)
	if result.Status != "completed" || result.Report == nil || result.Report.Verdict != "approve" || result.Auth != "google-oauth" {
		t.Fatalf("Gemini result = %#v", result)
	}
	if result.Report.Reviewer != "gemini" || result.Report.BaseSHA != "base" || result.Report.HeadSHA != "head" || result.ToolVersion != "0.46.0" {
		t.Fatalf("missing audited target or version: %#v", result)
	}
	if result.Usage.InputTokens != 100 || result.Usage.ThinkingTokens != 40 || result.Usage.TurnsKnown || result.Usage.APIEquivalentCostKnown {
		t.Fatalf("Gemini usage = %#v", result.Usage)
	}
	home, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(string(home))); !os.IsNotExist(err) {
		t.Fatalf("private Gemini session was not removed: %v", err)
	}
	for _, name := range []string{"gemini.effective-prompt.md", "gemini-system.effective-prompt.md", "gemini.raw.json", "gemini.stderr.log"} {
		info, err := os.Stat(filepath.Join(request.RunDir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("audit artifact %s not private: %v", name, err)
		}
	}
}

func TestGeminiSessionIgnoresUserAndRepositoryConfiguration(t *testing.T) {
	authHome := geminiTestAuth(t)
	request := geminiTestRequest(t)
	for _, path := range []string{filepath.Join(request.WorkDir, ".gemini"), filepath.Join(request.WorkDir, "nested")} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join(authHome, ".gemini", "settings.json"), filepath.Join(authHome, ".gemini", ".env"),
		filepath.Join(request.WorkDir, ".gemini", "settings.json"), filepath.Join(request.WorkDir, ".gemini", ".env"),
		filepath.Join(request.WorkDir, "GEMINI.md"), filepath.Join(request.WorkDir, "nested", "GEMINI.md"),
	} {
		if err := os.WriteFile(path, []byte("UNTRUSTED SETTINGS OR INSTRUCTIONS"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GENAI_USE_VERTEXAI", "GEMINI_SYSTEM_MD", "GEMINI_CLI_SYSTEM_SETTINGS_PATH", "NODE_OPTIONS", "CODE_ASSIST_ENDPOINT", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "host-injection")
	}
	session := t.TempDir()
	workspace, env, mapping, auth, err := prepareGeminiSession(session, config.Defaults().Reviewers.Gemini, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 2 || auth != "google-oauth" || workspace == request.WorkDir {
		t.Fatalf("source isolation = %q %#v %q", workspace, mapping, auth)
	}
	for _, entry := range env {
		if strings.Contains(entry, "host-injection") || strings.Contains(entry, authHome) {
			t.Fatalf("host configuration leaked into Gemini: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
	for _, original := range []string{"GEMINI.md", "nested/GEMINI.md"} {
		alias := filepath.Join(workspace, "source", filepath.FromSlash(mapping[original]))
		if data, err := os.ReadFile(alias); err != nil || string(data) != "UNTRUSTED SETTINGS OR INSTRUCTIONS" {
			t.Fatalf("instruction evidence not preserved: %v", err)
		}
		if _, err := os.Stat(filepath.Join(workspace, "source", original)); !os.IsNotExist(err) {
			t.Fatalf("auto-loaded instruction file still exists: %s", original)
		}
	}
	var settings map[string]any
	contents, err := os.ReadFile(filepath.Join(session, "settings.json"))
	if err != nil || json.Unmarshal(contents, &settings) != nil {
		t.Fatalf("generated settings unreadable: %v", err)
	}
	core := settings["tools"].(map[string]any)["core"]
	if !reflect.DeepEqual(core, []any{"list_directory", "read_file", "grep_search"}) {
		t.Fatalf("Gemini tools are not read-only: %#v", core)
	}
	if settings["hooksConfig"].(map[string]any)["enabled"] != false || settings["skills"].(map[string]any)["enabled"] != false {
		t.Fatal("hooks or skills are not disabled")
	}
	if _, err := os.Stat(filepath.Join(session, "home", ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("personal settings copied")
	}
}

func TestGeminiArgsBlockMCPDiscoveryWithNonemptyUniqueAllowlist(t *testing.T) {
	allowedName := func() string {
		args := geminiReviewArgs(config.Defaults().Reviewers.Gemini, "/session")
		for index, value := range args {
			if value == "--allowed-mcp-server-names" && index+1 < len(args) {
				return args[index+1]
			}
		}
		return ""
	}
	first, second := allowedName(), allowedName()
	if !strings.HasPrefix(first, "cora-disabled-") || first == second {
		t.Fatal("Gemini must not discover configured or admin-required MCP servers")
	}
}

func TestGeminiAuthenticationRequiresPersonalOAuthOrExplicitAPIKey(t *testing.T) {
	for _, test := range []struct {
		name, credentials, key string
		allow, wantOK          bool
		wantAuth               string
	}{
		{"oauth", `{"refresh_token":"fixture"}`, "", false, true, "google-oauth"},
		{"key forbidden", "", "fixture-key", false, false, ""},
		{"key authorized", "", "fixture-key", true, true, "gemini-api-key"},
		{"service account", `{"type":"service_account","access_token":"fixture"}`, "", false, false, ""},
		{"external account", `{"type":"external_account","refresh_token":"fixture"}`, "", false, false, ""},
		{"ADC account", `{"type":"authorized_user","refresh_token":"fixture"}`, "", false, false, ""},
		{"invalid", `not json`, "", false, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := geminiTestAuth(t)
			t.Setenv("GEMINI_API_KEY", test.key)
			t.Setenv("GOOGLE_API_KEY", "")
			if err := os.WriteFile(filepath.Join(source, ".gemini", "oauth_creds.json"), []byte(test.credentials), 0o600); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, ".gemini"), 0o700); err != nil {
				t.Fatal(err)
			}
			_, auth, _, err := prepareGeminiAuth(home, test.allow)
			if (err == nil) != test.wantOK || auth != test.wantAuth {
				t.Fatalf("auth = %q error = %v", auth, err)
			}
		})
	}
}

func TestGeminiReviewFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name, version, body, kind string
		partial                   bool
	}{
		{"old CLI", "0.45.0", "exit 99", "", false},
		{"malformed", "0.46.0", "printf '%s' 'not json'", "", false},
		{"missing required report fields", "0.46.0", `printf '%s' '{"response":"{\"schema_version\":\"1\",\"verdict\":\"approve\",\"context_complete\":true}"}'`, "", false},
		{"error despite zero exit", "0.46.0", `printf '%s' '{"error":{"message":"quota exhausted; retry in 5s"}}'`, "quota", false},
		{"typed quota despite zero exit", "0.46.0", `printf '%s' '{"error":{"type":"RESOURCE_EXHAUSTED","message":"Please retry in 5s"}}'`, "quota", false},
		{"quota stderr", "0.46.0", `printf '%s' '{"error":{"type":"RESOURCE_EXHAUSTED","message":"Please retry in 5s"}}' >&2; exit 1`, "quota", false},
		{"turn limit", "0.46.0", `printf '%s' '` + geminiSuccessEnvelope() + `'; exit 53`, "turn_limit", true},
		{"timeout", "0.46.0", "sleep 10", "timeout", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			geminiTestAuth(t)
			request := geminiTestRequest(t)
			request.Timeout = 100 * time.Millisecond
			cfg := config.Defaults().Reviewers.Gemini
			cfg.Command = geminiTestCLI(t, test.version, test.body)
			result := (Gemini{Config: cfg}).Review(context.Background(), request)
			if result.Status == "completed" || result.Error == "" || result.FailureKind != test.kind {
				t.Fatalf("failure result = %#v", result)
			}
			if test.partial && (result.Report == nil || result.Report.ContextComplete || result.Report.Verdict != "abstain") {
				t.Fatalf("partial report could approve: %#v", result)
			}
			if test.kind == "quota" && (!result.Retryable || result.RetryAt == nil) {
				t.Fatalf("quota did not preserve retry time: %#v", result)
			}
		})
	}
}

func TestGeminiPathNormalizationPreservesRepositorySourceDirectory(t *testing.T) {
	report := model.ReviewReport{
		Findings:      []model.Finding{{File: "source/app.go"}, {File: "/session/workspace/source/.context.txt"}},
		ReviewedPaths: []string{"source/app.go", "/session/workspace/source/app.go", ".context.txt"},
		OmittedPaths:  []string{"./other.go"},
	}
	normalizeGeminiReportPaths(&report, "/session/workspace", map[string]string{"GEMINI.md": ".context.txt"})
	if report.Findings[0].File != "source/app.go" || report.Findings[1].File != "GEMINI.md" || !reflect.DeepEqual(report.ReviewedPaths, []string{"source/app.go", "app.go", "GEMINI.md"}) {
		t.Fatalf("normalized report = %#v", report)
	}
}

func TestGeminiQuotaResetDurations(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for message, delay := range map[string]time.Duration{
		"Your quota will reset after 1h2m3s.":                 time.Hour + 2*time.Minute + 3*time.Second,
		"RESOURCE_EXHAUSTED: Please retry in 1.5s":            1500 * time.Millisecond,
		"You have exhausted your capacity. Retry after 500ms": 500 * time.Millisecond,
	} {
		retryAt, quota := QuotaRetryAt(message, now)
		if !quota || !retryAt.Equal(now.Add(delay)) {
			t.Fatalf("quota %q = %s, %t", message, retryAt, quota)
		}
	}
}

func TestGeminiRejectsOverridingSystemPolicies(t *testing.T) {
	directory := t.TempDir()
	if err := checkGeminiSystemPolicies(directory); err != nil {
		t.Fatal(err)
	}
	if err := checkGeminiSystemPolicies(filepath.Join(directory, "absent")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "admin.toml"), []byte("[[safety_checker]]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkGeminiSystemPolicies(directory); err == nil {
		t.Fatal("machine policy was silently allowed to override CORA policy")
	}
}
