package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestApplyReviewPolicyRoundTripsEffectiveConfiguration(t *testing.T) {
	cfg := Defaults()
	cfg.StrictPolicy = true
	cfg.AllowReviewWeb = true
	cfg.AllowUnsafeChecks = true
	cfg.Escalation.ForceSecuritySensitive = true
	cfg.Escalation.AdjudicateDisagreements = true
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.Command = "/opt/tools/gemini"
	cfg.Reviewers.Gemini.Model = "gemini-custom"
	cfg.Reviewers.Gemini.MaxTurns = 35
	cfg.Reviewers.Gemini.MaxConcurrency = 2
	cfg.MinimumApprovals = 3
	cfg.Checks = []Check{{
		Name: "go-test", Command: []string{"go", "test", "./..."}, Timeout: Duration{Duration: 7 * time.Minute},
		EnvAllowlist: []string{"GONOSUMDB"}, Profile: "go",
	}}
	policy := SnapshotReviewPolicy(cfg)

	restored, err := ApplyReviewPolicy(Defaults(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := SnapshotReviewPolicy(restored); !reflect.DeepEqual(got, policy) {
		t.Fatalf("restored policy = %#v, want %#v", got, policy)
	}
	if restored.ValidationProfiles != nil {
		t.Fatalf("restored policy retained unexpanded profiles: %#v", restored.ValidationProfiles)
	}
}

func TestApplyLegacyReviewPolicyKeepsGeminiDisabled(t *testing.T) {
	policy := SnapshotReviewPolicy(Defaults())
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "gemini")
	encoded, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var restoredPolicy model.AutoFixReviewPolicy
	if err := json.Unmarshal(encoded, &restoredPolicy); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.MinimumApprovals = 3
	restored, err := ApplyReviewPolicy(cfg, restoredPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Reviewers.Gemini.Enabled || restored.MinimumApprovals != 2 {
		t.Fatalf("legacy policy changed its reviewer requirements: %#v", restored)
	}
	if got := SnapshotReviewPolicy(restored); !reflect.DeepEqual(got, restoredPolicy) {
		t.Fatalf("legacy policy failed to round trip: got %#v, want %#v", got, restoredPolicy)
	}
}

func TestSnapshotReviewerExecutionLimitsIncludesGemini(t *testing.T) {
	cfg := Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.MaxTurns = 37
	cfg.ReviewerTimeout = Duration{Duration: 8 * time.Minute}
	limit := SnapshotReviewerExecutionLimits(cfg)["gemini"]
	if limit.MaxTurns != 37 || limit.Timeout.Duration != 8*time.Minute {
		t.Fatalf("Gemini execution limits = %#v", limit)
	}
}

func TestGeminiDefaultsPreserveTwoReviewerConsensus(t *testing.T) {
	cfg := Defaults()
	if cfg.Reviewers.Gemini.Enabled || !cfg.Reviewers.Codex.Enabled || !cfg.Reviewers.Claude.Enabled || cfg.MinimumApprovals != 2 {
		t.Fatalf("default consensus changed: reviewers %#v, minimum approvals %d", cfg.Reviewers, cfg.MinimumApprovals)
	}
	want := Reviewer{Command: "gemini", Model: "gemini-2.5-pro", MaxTurns: 50, MaxConcurrency: 1}
	if cfg.Reviewers.Gemini != want {
		t.Fatalf("Gemini defaults = %#v, want %#v", cfg.Reviewers.Gemini, want)
	}
}

func TestApplyRepositoryEnablesThreeReviewerConsensus(t *testing.T) {
	cfg, err := ApplyRepository(Defaults(), ".cora/config.toml", []byte(`
minimum_approvals = 3

[reviewers.gemini]
enabled = true
model = "gemini-custom"
max_turns = 40
max_concurrency = 2
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reviewers.Gemini.Enabled || cfg.Reviewers.Gemini.Command != "gemini" || cfg.Reviewers.Gemini.Model != "gemini-custom" || cfg.Reviewers.Gemini.MaxTurns != 40 || cfg.Reviewers.Gemini.MaxConcurrency != 2 || cfg.MinimumApprovals != 3 {
		t.Fatalf("Gemini configuration not merged: %#v", cfg)
	}
}

func TestValidateGeminiOnlyReviewer(t *testing.T) {
	cfg := Defaults()
	cfg.Reviewers.Codex.Enabled = false
	cfg.Reviewers.Claude.Enabled = false
	cfg.Reviewers.Gemini.Enabled = true
	cfg.MinimumApprovals = 1
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Gemini-only consensus should be valid: %v", err)
	}
	cfg.MinimumApprovals = 2
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "the 1 enabled reviewers") {
		t.Fatalf("Gemini-only minimum approvals error = %v", err)
	}
}

func TestValidateRejectsInvalidGeminiControls(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Reviewer)
		field  string
	}{
		{"empty command", func(reviewer *Reviewer) { reviewer.Command = " " }, "command"},
		{"empty model", func(reviewer *Reviewer) { reviewer.Model = " " }, "model"},
		{"zero turns", func(reviewer *Reviewer) { reviewer.MaxTurns = 0 }, "max_turns"},
		{"negative turns", func(reviewer *Reviewer) { reviewer.MaxTurns = -1 }, "max_turns"},
		{"zero concurrency", func(reviewer *Reviewer) { reviewer.MaxConcurrency = 0 }, "max_concurrency"},
		{"effort", func(reviewer *Reviewer) { reviewer.Effort = "high" }, "effort"},
		{"finalization turns", func(reviewer *Reviewer) { reviewer.FinalizationTurns = 2 }, "finalization_turns"},
		{"budget", func(reviewer *Reviewer) { reviewer.MaxBudgetUSD = 5 }, "max_budget_usd"},
		{"negative budget", func(reviewer *Reviewer) { reviewer.MaxBudgetUSD = -1 }, "max_budget_usd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Reviewers.Gemini.Enabled = true
			test.mutate(&cfg.Reviewers.Gemini)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reviewers.gemini."+test.field) {
				t.Fatalf("Gemini %s validation error = %v", test.name, err)
			}
		})
	}
}

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

func TestDefaultsUseHighEffortClaudeOpusAndTargetedFable(t *testing.T) {
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
	if cfg.AllowReviewWeb {
		t.Fatal("review web authorization must default off")
	}
	if !cfg.CrossExamineBlockingFindings {
		t.Fatal("targeted blocking-finding cross-examination should default on")
	}
	if cfg.CrossExamination.MaxTurns != 20 || cfg.CrossExamination.MaxBudgetUSD != 5 || cfg.CrossExamination.Timeout.Duration != 10*time.Minute {
		t.Fatalf("cross-examination defaults = %#v", cfg.CrossExamination)
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

func TestApplyRepositoryDecodesReviewWebAuthorization(t *testing.T) {
	cfg, err := ApplyRepository(Defaults(), ".cora/config.toml", []byte("allow_review_web = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowReviewWeb {
		t.Fatal("trusted configuration did not enable review web evidence")
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

func TestApplyRepositoryDecodesIndependentCrossExaminationBudget(t *testing.T) {
	cfg, err := ApplyRepository(Defaults(), ".cora/config.toml", []byte(`
[cross_examination]
timeout = "8m"
max_turns = 16
max_budget_usd = 3.5
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrossExamination.Timeout.Duration != 8*time.Minute || cfg.CrossExamination.MaxTurns != 16 || cfg.CrossExamination.MaxBudgetUSD != 3.5 {
		t.Fatalf("cross-examination budget = %#v", cfg.CrossExamination)
	}
}

func TestValidateRejectsInvalidCrossExaminationBudget(t *testing.T) {
	cfg := Defaults()
	cfg.CrossExamination.MaxTurns = cfg.Reviewers.Claude.FinalizationTurns
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cross_examination.max_turns") {
		t.Fatalf("cross-examination turn limit error = %v", err)
	}
	cfg = Defaults()
	cfg.CrossExamination.MaxBudgetUSD = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cross_examination.max_budget_usd") {
		t.Fatalf("cross-examination budget error = %v", err)
	}
	cfg = Defaults()
	cfg.CrossExamination.Timeout.Duration = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cross_examination.timeout") {
		t.Fatalf("cross-examination timeout error = %v", err)
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
	if cfg.AllowReviewWeb {
		t.Fatal("example configuration should leave review web authorization disabled")
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
