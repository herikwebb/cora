package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
