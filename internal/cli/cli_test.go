package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
)

func TestRootCommandUsesReviewInsteadOfRun(t *testing.T) {
	root := newRootCommand()
	names := make([]string, 0, len(root.Commands()))
	for _, command := range root.Commands() {
		names = append(names, command.Name())
	}

	if !slices.Contains(names, "review") {
		t.Fatalf("root commands %v do not include review", names)
	}
	if slices.Contains(names, "run") {
		t.Fatalf("root commands %v still include legacy run command", names)
	}
	if !slices.Contains(names, "config") {
		t.Fatalf("root commands %v do not include config", names)
	}
	for _, name := range []string{"retry", "list", "status", "show"} {
		if !slices.Contains(names, name) {
			t.Fatalf("root commands %v do not include %s", names, name)
		}
	}
}

func TestPrintActiveRunsShowsConcurrentReviewerElapsedTime(t *testing.T) {
	var output bytes.Buffer
	printActiveRuns(&output, []model.RunSummary{
		{RunID: "run-one", HeadSHA: "aaaaaaaaaa", ElapsedMS: 65_000, Phase: "reviewers", Reviewers: map[string]string{"codex": "running"}, ReviewerElapsedMS: map[string]int64{"codex": 42_000}},
		{RunID: "run-two", HeadSHA: "bbbbbbbbbb", ElapsedMS: 30_000, Phase: "reviewers", Reviewers: map[string]string{"claude": "queued"}, Queues: map[string]model.ProviderQueueStatus{"claude": {Position: 2}}},
	})
	text := output.String()
	for _, want := range []string{"run-one", "run-two", "codex=running(42s)", "claude=queued#2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("active output does not contain %q:\n%s", want, text)
		}
	}
}

func TestPrintConsolidatedDetailsIncludesEvidenceFixesAndRisks(t *testing.T) {
	var output bytes.Buffer
	printConsolidatedDetails(&output, model.Decision{
		Findings: []model.ConsolidatedFinding{{
			Severity: "minor", Confidence: 0.8, File: "app.go", Line: 12, Claim: "leak",
			Evidence: []string{"handle is never closed"}, SuggestedFixes: []string{"defer handle.Close()"}, Reviewers: []string{"codex"},
		}},
		ResidualRisks: []string{"integration test not run"},
	})
	text := output.String()
	for _, want := range []string{"Confidence: 80%", "Evidence: handle is never closed", "Suggested fix: defer handle.Close()", "integration test not run"} {
		if !strings.Contains(text, want) {
			t.Fatalf("human details do not contain %q:\n%s", want, text)
		}
	}
}

func TestPrintConsolidatedDetailsExplainsDisprovedFinding(t *testing.T) {
	var output bytes.Buffer
	printConsolidatedDetails(&output, model.Decision{
		RejectedFindings: []model.ConsolidatedFinding{{
			ID: "cora-123", OriginalSeverity: "major", Confidence: 0.95,
			File: "handler.go", Line: 20, Claim: "input reaches sink", Evidence: []string{"handler calls runner"},
		}},
		CrossExaminations: []model.CrossExamination{{
			FindingID: "cora-123", Reviewer: "claude-cross-examination", Status: "completed",
			Disposition: "disproved", OriginalSeverity: "major", EffectiveSeverity: "note",
			Rationale:    "validation replaces the input before the call",
			Reachability: &model.Reachability{Status: "not_demonstrated", Path: []string{"handler.go:18 validates", "runner.go:40 receives constant"}, Impact: "attacker input never reaches execution"},
		}},
	})
	text := output.String()
	for _, want := range []string{"Disproved findings", "Original evidence: handler calls runner", "disproved by claude-cross-examination", "validation replaces the input", "handler.go:18 validates -> runner.go:40 receives constant", "attacker input never reaches execution"} {
		if !strings.Contains(text, want) {
			t.Fatalf("disproved finding details do not contain %q:\n%s", want, text)
		}
	}
}

func TestExpandAutoProfilesDetectsCommonProjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	for _, path := range []string{"go.mod", "package.json", "pyproject.toml"} {
		writeCLIFile(t, filepath.Join(root, path), "test\n")
	}
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "chore: add project markers")
	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := expandAutoProfiles(ctx, repo, model.Target{HeadSHA: head}, []string{"auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profiles, []string{"go", "node", "python"}) {
		t.Fatalf("auto profiles = %v", profiles)
	}
}

func TestSelectRetryReviewersDefaultsToIncompleteProviders(t *testing.T) {
	results := []model.ReviewerResult{
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve"}},
		{Reviewer: "claude", Status: "incomplete", FailureKind: "quota"},
	}
	selected, err := selectRetryReviewers(results, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || !selected["claude"] || selected["codex"] {
		t.Fatalf("selected reviewers = %#v", selected)
	}
}

func TestQuotaNotBeforeSelectsOnlyRequestedReviewer(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 30, 0, 0, time.FixedZone("EDT", -4*60*60))
	retryAt := now.Add(time.Hour)
	results := []model.ReviewerResult{
		{Reviewer: "claude", Retryable: true, RetryAt: &retryAt},
		{Reviewer: "codex", Retryable: true, RetryAt: &retryAt},
	}
	notBefore := quotaNotBefore(results, map[string]bool{"claude": true}, now, now)
	if len(notBefore) != 1 || !notBefore["claude"].Equal(retryAt) {
		t.Fatalf("quota queue = %#v", notBefore)
	}
}

func TestQuotaNotBeforeRecoversResetFromLegacyReviewerError(t *testing.T) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 8, 25, 11, 22, 0, 0, eastern)
	now := time.Date(2026, 8, 25, 11, 30, 0, 0, eastern)
	results := []model.ReviewerResult{{
		Reviewer: "claude", Status: "incomplete",
		Error: "Claude review failed: You've hit your session limit · resets 11:50am (America/New_York)",
	}}

	notBefore := quotaNotBefore(results, map[string]bool{"claude": true}, observedAt, now)
	want := time.Date(2026, 8, 25, 11, 50, 0, 0, eastern)
	if got := notBefore["claude"]; !got.Equal(want) {
		t.Fatalf("recovered retry time = %s, want %s", got, want)
	}
}

func TestExitCodeForState(t *testing.T) {
	tests := map[string]int{
		model.StateApproved:         0,
		model.StateChangesRequested: 2,
		model.StateNeedsHuman:       3,
		model.StateIncomplete:       4,
		model.StateStale:            5,
		"unknown":                   10,
	}
	for state, want := range tests {
		if got := exitCodeForState(state); got != want {
			t.Errorf("exitCodeForState(%q) = %d, want %d", state, got, want)
		}
	}
}

func TestLoadTrustedConfigIgnoresHeadConfiguration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	writeCLIFile(t, filepath.Join(root, "app.txt"), "base\n")
	writeCLIFile(t, filepath.Join(root, ".cora", "config.toml"), `
minimum_approvals = 1
[reviewers.claude]
enabled = false
`)
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "chore: initialize trusted base")
	gitCLI(t, root, "switch", "-c", "feature")
	writeCLIFile(t, filepath.Join(root, "app.txt"), "base\nfeature\n")
	writeCLIFile(t, filepath.Join(root, ".cora", "config.toml"), `
minimum_approvals = 2
[reviewers.claude]
enabled = true
[[checks]]
name = "exfiltrate"
command = ["sh", "-c", "env"]
`)
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "feat: add untrusted change")

	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveTarget(ctx, gitx.TargetOptions{Base: "main", RequireClean: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadTrustedConfig(ctx, repo, config.Defaults(), target)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reviewers.Claude.Enabled || cfg.MinimumApprovals != 1 || len(cfg.Checks) != 0 {
		t.Fatalf("head config influenced effective config: %#v", cfg)
	}
}

func gitCLI(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeCLIFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
