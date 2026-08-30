package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadLayersDefaultsUserAndRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(userConfigDir, "cora", "config.toml"), `
reviewer_timeout = "7m"
minimum_approvals = 1

[reviewers.claude]
enabled = false

[[checks]]
name = "personal"
command = ["true"]
`)

	repo := t.TempDir()
	writeTestFile(t, filepath.Join(repo, ".cora", "config.toml"), `
base = "upstream/main"
minimum_approvals = 2

[reviewers.claude]
enabled = true
max_turns = 8

[[checks]]
name = "unit"
command = ["go", "test", "./..."]
`)

	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Base != "upstream/main" || cfg.ReviewerTimeout.Duration != 7*time.Minute {
		t.Fatalf("layered config not applied: %#v", cfg)
	}
	if !cfg.Reviewers.Codex.Enabled || !cfg.Reviewers.Claude.Enabled || cfg.Reviewers.Claude.MaxTurns != 8 {
		t.Fatalf("reviewer config not merged: %#v", cfg.Reviewers)
	}
	if cfg.MinimumApprovals != 2 {
		t.Fatalf("minimum approvals = %d", cfg.MinimumApprovals)
	}
	if len(cfg.Checks) != 1 || cfg.Checks[0].Timeout.Duration != 10*time.Minute {
		t.Fatalf("check defaults not applied: %#v", cfg.Checks)
	}
	if len(cfg.LoadedFiles) != 2 {
		t.Fatalf("loaded files = %v", cfg.LoadedFiles)
	}
}

func TestDefaultsUseHighEffortClaudeOpusAndFableEscalation(t *testing.T) {
	cfg := Defaults()
	if cfg.Reviewers.Codex.Model != "gpt-5.6-sol" || cfg.Reviewers.Codex.Effort != "high" {
		t.Fatalf("Codex defaults = model %q effort %q", cfg.Reviewers.Codex.Model, cfg.Reviewers.Codex.Effort)
	}
	if cfg.Reviewers.Claude.Model != "opus" || cfg.Reviewers.Claude.Effort != "high" {
		t.Fatalf("Claude defaults = model %q effort %q", cfg.Reviewers.Claude.Model, cfg.Reviewers.Claude.Effort)
	}
	if !cfg.Escalation.Enabled || cfg.Escalation.Model != "fable" || cfg.Escalation.Effort != "high" {
		t.Fatalf("escalation defaults = %#v", cfg.Escalation)
	}
	if cfg.Escalation.MaxTurns != nil || cfg.Escalation.MaxBudgetUSD != nil {
		t.Fatalf("escalation limits should inherit Claude reviewer limits by default: %#v", cfg.Escalation)
	}
	if cfg.Reviewers.Claude.MaxConcurrency != 1 || cfg.Reviewers.Codex.MaxConcurrency != 2 {
		t.Fatalf("provider concurrency defaults = %#v", cfg.Reviewers)
	}
	if cfg.Reviewers.Claude.FinalizationTurns != 2 {
		t.Fatalf("Claude finalization reserve = %d", cfg.Reviewers.Claude.FinalizationTurns)
	}
	if cfg.Escalation.AdjudicateDisagreements {
		t.Fatal("disagreement adjudication should require explicit opt-in")
	}
	if !cfg.CrossExamineBlockingFindings {
		t.Fatal("targeted blocking-finding cross-examination should default on")
	}
	if cfg.AutoFix.Command != "codex" || cfg.AutoFix.Model != "gpt-5.6-sol" || cfg.AutoFix.Effort != "high" || cfg.AutoFix.Threshold != "major" {
		t.Fatalf("auto-fix defaults = %#v", cfg.AutoFix)
	}
	if cfg.AutoFix.MaxIterations != 5 || cfg.AutoFix.MaxTurns != 250 || cfg.AutoFix.MaxCostUSD != 50 || cfg.AutoFix.MaxDuration.Duration <= 0 || cfg.AutoFix.AgentTimeout.Duration <= 0 {
		t.Fatalf("auto-fix limits = %#v", cfg.AutoFix)
	}
}

func TestApplyRepositoryDecodesEscalationLimitOverrides(t *testing.T) {
	cfg, err := ApplyRepository(Defaults(), ".cora/config.toml", []byte(`
[escalation]
max_turns = 40
max_budget_usd = 6.5
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalation.MaxTurns == nil || *cfg.Escalation.MaxTurns != 40 {
		t.Fatalf("escalation max turns = %#v", cfg.Escalation.MaxTurns)
	}
	if cfg.Escalation.MaxBudgetUSD == nil || *cfg.Escalation.MaxBudgetUSD != 6.5 {
		t.Fatalf("escalation max budget = %#v", cfg.Escalation.MaxBudgetUSD)
	}
}

func TestValidateEscalationLimitOverrides(t *testing.T) {
	maxTurns := 0
	cfg := Defaults()
	cfg.Escalation.MaxTurns = &maxTurns
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "escalation.max_turns must be positive") {
		t.Fatalf("zero escalation turn limit error = %v", err)
	}

	maxTurns = cfg.Reviewers.Claude.FinalizationTurns
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "greater than reviewers.claude.finalization_turns") {
		t.Fatalf("escalation finalization reserve error = %v", err)
	}

	maxTurns = cfg.Reviewers.Claude.FinalizationTurns + 1
	maxBudget := -1.0
	cfg.Escalation.MaxBudgetUSD = &maxBudget
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "escalation.max_budget_usd") {
		t.Fatalf("negative escalation budget error = %v", err)
	}

	maxBudget = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicitly disabling the escalation budget should be valid: %v", err)
	}

	cfg.Escalation.Enabled = false
	maxTurns = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "escalation.max_turns must be positive") {
		t.Fatalf("cross-examination must validate escalation limits even when broad escalation is disabled: %v", err)
	}
}

func TestValidateRejectsInvalidAutoFixLimits(t *testing.T) {
	cfg := Defaults()
	cfg.AutoFix.Threshold = "note"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "auto_fix.until") {
		t.Fatalf("invalid auto-fix threshold error = %v", err)
	}
	cfg = Defaults()
	cfg.AutoFix.MaxIterations = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_iterations") {
		t.Fatalf("invalid auto-fix iteration error = %v", err)
	}
}

func TestApplyBuiltInGoValidationProfile(t *testing.T) {
	cfg, err := ApplyProfiles(Defaults(), []string{"go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Checks) != 2 || cfg.Checks[0].Profile != "go" || cfg.Checks[1].Profile != "go" {
		t.Fatalf("Go profile checks = %#v", cfg.Checks)
	}
	if cfg.Checks[0].Name != "go-test" || cfg.Checks[1].Name != "go-vet" {
		t.Fatalf("Go profile check names = %#v", cfg.Checks)
	}
}

func TestApplyBuiltInNodeAndPythonValidationProfiles(t *testing.T) {
	cfg, err := ApplyProfiles(Defaults(), []string{"node", "python"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Checks) != 2 || cfg.Checks[0].Name != "node-test" || cfg.Checks[1].Name != "python-test" {
		t.Fatalf("built-in checks = %#v", cfg.Checks)
	}
}

func TestExampleConfigurationParses(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ApplyRepository(Defaults(), "examples/config.toml", contents)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ValidationProfiles) != 1 || cfg.ValidationProfiles[0].Name != "go-fast" {
		t.Fatalf("validation profiles = %#v", cfg.ValidationProfiles)
	}
}

func TestValidateRejectsUnsupportedReviewerEffort(t *testing.T) {
	cfg := Defaults()
	cfg.Reviewers.Claude.Effort = "maximum"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reviewers.claude.effort") {
		t.Fatalf("expected invalid Claude effort error, got %v", err)
	}

	cfg = Defaults()
	cfg.Reviewers.Codex.Effort = "maximum"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reviewers.codex.effort") {
		t.Fatalf("expected invalid Codex effort error, got %v", err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := t.TempDir()
	writeTestFile(t, filepath.Join(repo, ".cora", "config.toml"), "minimum_approvalz = 2\n")

	_, err := Load(repo)
	if err == nil || !strings.Contains(err.Error(), "minimum_approvalz") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}

func TestUserPathUsesOperatingSystemConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	wantDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wantDir, "cora", "config.toml")
	got, err := UserPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("UserPath() = %q, want %q", got, want)
	}
}

func TestValidateRejectsInvalidCheckEnvironmentAllowlist(t *testing.T) {
	cfg := Defaults()
	cfg.Checks = []Check{{
		Name:         "unit",
		Command:      []string{"true"},
		Timeout:      Duration{Duration: time.Second},
		EnvAllowlist: []string{"VALID_NAME", "AWS-SECRET"},
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "AWS-SECRET") {
		t.Fatalf("expected invalid environment variable error, got %v", err)
	}
}

func TestApplyRepositoryLoadsTrustedContents(t *testing.T) {
	cfg, err := ApplyRepository(Defaults(), "git:base:.cora/config.toml", []byte(`
minimum_approvals = 1

[reviewers.claude]
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reviewers.Claude.Enabled || cfg.MinimumApprovals != 1 {
		t.Fatalf("trusted repository config not applied: %#v", cfg)
	}
	if len(cfg.LoadedFiles) != 1 || cfg.LoadedFiles[0] != "git:base:.cora/config.toml" {
		t.Fatalf("loaded files = %v", cfg.LoadedFiles)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
