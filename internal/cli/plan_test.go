package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
)

func TestPlanCommandExposesReviewPolicyFlags(t *testing.T) {
	command := newPlanCommand(&options{})
	for _, name := range []string{
		"base", "commit", "range", "uncommitted", "parent", "allow-api-billing",
		"allow-review-web", "allow-unsafe-checks", "security-sensitive", "adjudicate", "strict", "profile", "validation-evidence", "web-evidence",
	} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("plan command is missing --%s", name)
		}
	}
}

func TestBuildReviewPlanUsesTrustedBaseAndDoesNotCreateRunState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	sentinel := filepath.Join(root, "plan-command-ran")
	commandPath := filepath.Join(root, "must-not-run.sh")
	writeCLIFile(t, commandPath, "#!/bin/sh\ntouch "+sentinel+"\n")
	if err := os.Chmod(commandPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "go.mod"), "module example.invalid/plan\n\ngo 1.25\n")
	writeCLIFile(t, filepath.Join(root, "app.go"), "package plan\n")
	writeCLIFile(t, filepath.Join(root, ".cora", "reviewer.md"), "trusted prompt\n")
	writeCLIFile(t, filepath.Join(root, ".cora", "config.toml"), fmt.Sprintf(`
reviewer_timeout = "12m"
overall_timeout = "35m"
queue_timeout = "2h"
minimum_approvals = 2
prompt_file = ".cora/reviewer.md"

[reviewers.codex]
command = %q
model = "trusted-codex"
effort = "medium"
max_concurrency = 3

[reviewers.claude]
command = %q
model = "trusted-opus"
effort = "high"
max_turns = 24
finalization_turns = 2
max_concurrency = 1

[escalation]
enabled = false
model = "trusted-fable"
effort = "high"
adjudicate_disagreements = false
security_path_markers = ["/auth/"]

[[checks]]
name = "sentinel"
command = [%q]
timeout = "1m"
env_allowlist = ["PATH"]
`, commandPath, commandPath, commandPath))
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "chore: initialize trusted base")
	gitCLI(t, root, "switch", "-c", "feature")
	writeCLIFile(t, filepath.Join(root, "auth", "token.go"), "package auth\n")
	writeCLIFile(t, filepath.Join(root, "AGENTS.md"), "untrusted instructions\n")
	writeCLIFile(t, filepath.Join(root, ".cora", "config.toml"), `
minimum_approvals = 1
[reviewers.codex]
model = "untrusted-head-model"
[reviewers.claude]
enabled = false
`)
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "feat: change authentication")

	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(repo.CommonDir, "cora")
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected preexisting run state at %s: %v", statePath, err)
	}
	expectedTarget, err := repo.ResolveTarget(ctx, gitx.TargetOptions{Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := repo.StableIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evidenceContents, err := json.Marshal(map[string]any{
		"schema_version": "1", "name": "ci", "repository_identity": identity,
		"base_sha": expectedTarget.BaseSHA, "head_sha": expectedTarget.HeadSHA, "diff_hash": expectedTarget.DiffHash,
		"status": "passed", "verified_at": time.Now().Add(-time.Minute).UTC(), "verifier": "test-ci",
		"source": "https://ci.example.invalid/run/1", "command": []string{"go", "test", "./..."}, "summary": "all tests passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(t.TempDir(), "ci.json")
	writeCLIFile(t, evidencePath, string(evidenceContents))
	indexPath := filepath.Join(repo.CommonDir, "index")
	changedTime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "app.go"), changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildReviewPlan(ctx, repo, config.Defaults(), planOptions{
		Base: "main", AllowReviewWeb: true, AllowUnsafeChecks: true, Strict: true, Profiles: []string{"auto"}, Adjudicate: true,
		ValidationEvidence: []string{evidencePath},
		WebEvidence:        []string{"https://docs.example.com/reference"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only plan created run state at %s: %v", statePath, err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("read-only plan refreshed or rewrote the Git index")
	}

	if plan.Target.Mode != "branch" || plan.Target.BaseRef != "main" || plan.Target.DiffHash == "" || len(plan.ChangedPaths) != 3 {
		t.Fatalf("planned target = %#v, paths = %v", plan.Target, plan.ChangedPaths)
	}
	if plan.Policy.MinimumApprovals != 2 || !plan.Policy.AllowReviewWeb || !slices.Contains(plan.Policy.BlockingSeverities, "minor") || plan.Policy.OverallTimeout.Duration != 35*time.Minute || !strings.Contains(plan.Policy.PromptSource, ".cora/reviewer.md") {
		t.Fatalf("planned policy = %#v", plan.Policy)
	}
	if plan.WebEvidence.Status != "planned" || len(plan.WebEvidence.URLs) != 1 || plan.WebEvidence.URLs[0] != "https://docs.example.com/reference" ||
		plan.WebEvidence.MaximumSources != 4 || plan.WebEvidence.PerSourceBytes != 32<<10 || plan.WebEvidence.TotalBytes != 64<<10 {
		t.Fatalf("planned web evidence = %#v", plan.WebEvidence)
	}
	codex := findPlannedReviewer(t, plan.Reviewers, "codex")
	if codex.Model != "trusted-codex" || codex.Effort != "medium" || codex.Timeout.Duration != 12*time.Minute || codex.MaxConcurrency != 3 || !codex.Scheduled {
		t.Fatalf("planned Codex reviewer = %#v", codex)
	}
	claude := findPlannedReviewer(t, plan.Reviewers, "claude")
	if claude.Model != "trusted-opus" || claude.MaxTurns != 24 || claude.FinalizationTurns != 2 || !claude.Scheduled {
		t.Fatalf("planned Claude reviewer = %#v", claude)
	}
	security := findPlannedReviewer(t, plan.Reviewers, "claude-security")
	if !security.Scheduled || !security.Required || security.Model != "trusted-fable" || security.MaxTurns != 24 {
		t.Fatalf("planned security reviewer = %#v", security)
	}
	adjudicator := findPlannedReviewer(t, plan.Reviewers, "claude-escalation")
	if !adjudicator.Enabled || !adjudicator.Conditional || adjudicator.Scheduled {
		t.Fatalf("planned adjudicator = %#v", adjudicator)
	}
	if !plan.Security.Triggered || !slices.Contains(plan.Security.ControlFiles, "AGENTS.md") || !slices.Contains(plan.Security.SensitivePaths, "auth/token.go") {
		t.Fatalf("security plan = %#v", plan.Security)
	}
	if plan.Validation.Status != "planned_with_imported_evidence" || !slices.Equal(plan.Validation.SelectedProfiles, []string{"go"}) || len(plan.Validation.Checks) != 3 || len(plan.Validation.ImportedEvidence) != 1 {
		t.Fatalf("validation plan = %#v", plan.Validation)
	}
	if evidence := plan.Validation.ImportedEvidence[0]; evidence.CheckName != "evidence:ci" || evidence.Status != "passed" || evidence.Attestation == nil || evidence.Attestation.Trust != "operator-supplied-attestation" {
		t.Fatalf("planned imported evidence = %#v", evidence)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only plan invoked a configured reviewer or check: %v", err)
	}
	if !plan.Ready || len(plan.BlockingIssues) != 0 {
		t.Fatalf("plan readiness = %t, issues = %v", plan.Ready, plan.BlockingIssues)
	}
	if capacity := findPlannedCapacity(t, plan.Capacity, "codex"); capacity.MaxConcurrency != 3 || capacity.InitialDemand != 1 || capacity.CurrentAvailability != "unknown until provider-slot acquisition" {
		t.Fatalf("Codex capacity = %#v", capacity)
	}
}

func TestExplicitEscalationFlagsOverrideDisabledTrustedPolicy(t *testing.T) {
	security := config.Defaults()
	security.Escalation.Enabled = false
	applyReviewPolicyOverrides(&security, reviewPolicyOverrides{SecuritySensitive: true})
	if !security.Escalation.Enabled || !security.Escalation.ForceSecuritySensitive {
		t.Fatalf("security-sensitive override = %#v", security.Escalation)
	}

	adjudication := config.Defaults()
	adjudication.Escalation.Enabled = false
	applyReviewPolicyOverrides(&adjudication, reviewPolicyOverrides{Adjudicate: true})
	if !adjudication.Escalation.Enabled || !adjudication.Escalation.AdjudicateDisagreements {
		t.Fatalf("adjudication override = %#v", adjudication.Escalation)
	}
}

func TestBuildReviewPlanRejectsMissingTrustedPrompt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	writeCLIFile(t, filepath.Join(root, "app.txt"), "base\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "chore: initialize base")
	gitCLI(t, root, "switch", "-c", "feature")
	writeCLIFile(t, filepath.Join(root, "app.txt"), "base\nfeature\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "feat: change app")
	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	personal := config.Defaults()
	personal.PromptFile = "missing-reviewer.md"
	_, err = buildReviewPlan(ctx, repo, personal, planOptions{Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "does not exist at base") {
		t.Fatalf("missing trusted prompt error = %v", err)
	}
}

func TestBuildUncommittedPlanDoesNotRefreshGitIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	writeCLIFile(t, filepath.Join(root, "clean.txt"), "clean\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "chore: initialize base")
	writeCLIFile(t, filepath.Join(root, "untracked.txt"), "review me\n")
	changedTime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "clean.txt"), changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(repo.CommonDir, "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildReviewPlan(ctx, repo, config.Defaults(), planOptions{Uncommitted: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("uncommitted read-only plan refreshed or rewrote the Git index")
	}
	if plan.Target.Mode != "uncommitted" || !slices.Contains(plan.ChangedPaths, "untracked.txt") {
		t.Fatalf("uncommitted plan = %#v, paths=%v", plan.Target, plan.ChangedPaths)
	}
}

func TestBuildReviewPlanTriggersSecurityForRenamedSensitiveSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CORA Test")
	gitCLI(t, root, "config", "user.email", "cora@example.invalid")
	writeCLIFile(t, filepath.Join(root, "auth", "token.go"), "package auth\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "feat(auth): add token validation")
	gitCLI(t, root, "switch", "-c", "rename-auth")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, root, "mv", "auth/token.go", "docs/token.go")
	gitCLI(t, root, "commit", "-m", "refactor(auth): relocate token validation")

	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildReviewPlan(ctx, repo, config.Defaults(), planOptions{Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Security.Triggered || !slices.Contains(plan.Security.SensitivePaths, "auth/token.go") {
		t.Fatalf("renamed sensitive source did not trigger security review: %#v", plan.Security)
	}
	security := findPlannedReviewer(t, plan.Reviewers, "claude-security")
	if !security.Scheduled || !security.Required {
		t.Fatalf("renamed sensitive source security reviewer = %#v", security)
	}
}

func TestPlanPreflightExplainsApprovalBlockersAndWarnings(t *testing.T) {
	cfg := config.Defaults()
	cfg.StrictPolicy = true
	cfg.Checks = nil
	cfg.Reviewers.Claude.Enabled = false
	target := model.Target{Mode: "branch", Dirty: true, Finalizable: false}
	blocking, warnings := planPreflight(cfg, target, true, 0, 1)
	for _, want := range []string{"working tree is not clean", "strict policy requires", "targeted security review", "web evidence requires"} {
		if !containsSubstring(blocking, want) {
			t.Fatalf("blocking issues %v do not contain %q", blocking, want)
		}
	}
	if !containsSubstring(warnings, "not finalizable") {
		t.Fatalf("warnings %v do not explain finalizability", warnings)
	}
}

func TestReviewPlanHumanAndJSONOutputIncludeEffectiveLimits(t *testing.T) {
	plan := reviewPlan{
		SchemaVersion: "1", Repository: "/repo", RepositoryIdentity: "example.invalid/repo",
		Target:       model.Target{Mode: "branch", BaseRef: "main", HeadRef: "HEAD", BaseSHA: "base", HeadSHA: "head", DiffHash: "diff", Finalizable: true},
		ChangedPaths: []string{"auth/token.go"}, Ready: true,
		Policy:    planPolicy{MinimumApprovals: 2, BlockingSeverities: []string{"blocker", "major"}, OverallTimeout: model.NewDuration(45 * time.Minute), QueueTimeout: model.NewDuration(time.Hour), RequireCleanTree: true, AllowReviewWeb: true, PromptSource: "embedded:prompts/default-review.md"},
		Reviewers: []planReviewer{{Name: "claude", Provider: "claude", Scheduled: true, Command: "claude path\nBLOCKED: forged", Model: "opus", ModelSource: "configured", Effort: "high", Timeout: model.NewDuration(15 * time.Minute), MaxTurns: 50, FinalizationTurns: 2, MaxBudgetUSD: 5, MaxConcurrency: 1}},
		Security:  planSecurity{Enabled: true, Triggered: true, Model: "fable", Effort: "high", SensitivePaths: []string{"auth/token.go"}},
		Validation: planValidation{
			Status: "planned_with_imported_evidence", SelectedProfiles: []string{"go"}, HostExecutionAuthorized: true, Isolation: "unsandboxed host",
			Checks: []planCheck{{
				Name: "go-test\nBLOCKED: forged", Command: []string{"go", "argument with spaces", "line\nbreak", "\x1b[31m"},
				Timeout: model.NewDuration(10 * time.Minute), EnvAllowlist: []string{"PATH"},
			}},
			ImportedEvidence: []planImportedEvidence{{
				CheckName: "evidence:ci", Status: "passed", Isolation: "imported-evidence-no-execution",
				Attestation: &model.ImportedValidationEvidence{
					Verifier: "CI\nBLOCKED: forged", Source: "run-1\nWARNING: forged", Trust: "operator-supplied-attestation",
					Command: []string{"go", "test", "./..."}, Summary: "passed\nBLOCKED: forged",
				},
			}},
		},
		WebEvidence: planWebEvidence{Status: "planned", Authorized: true, URLs: []string{"https://docs.example.com/reference"}, MaximumSources: 4, PerSourceBytes: 32768, TotalBytes: 65536, Isolation: "reviewers remain offline"},
		Capacity:    []planProviderCapacity{{Provider: "claude", MaxConcurrency: 1, InitialDemand: 1, QueueTimeout: model.NewDuration(time.Hour), CurrentAvailability: "unknown until provider-slot acquisition"}},
	}
	var human bytes.Buffer
	printReviewPlan(&human, plan)
	for _, want := range []string{"CORA REVIEW PLAN", "Ready: yes", "claude", `command="claude path\nBLOCKED: forged"`, "model=opus", "max_turns=50", "finalization_turns=2", "max_budget_usd=5.00", "clean_tree=true", "review_web=true", "prompt=embedded:", "Security: triggered", "host_execution_authorized=true", "unsandboxed host", `env_allowlist=["PATH"]`, "go-test", `"argument with spaces"`, `line\nbreak`, `\x1b[31m`, "evidence:ci", "operator-supplied-attestation", "Web evidence: planned", `url="https://docs.example.com/reference"`, "reviewers remain offline", "global_limit=1", "availability=unknown"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human plan does not contain %q:\n%s", want, human.String())
		}
	}
	if strings.Contains(human.String(), "\nBLOCKED: forged") || strings.Contains(human.String(), "\nWARNING: forged") {
		t.Fatalf("untrusted plan data injected terminal lines:\n%s", human.String())
	}

	var encoded bytes.Buffer
	if err := writePlanJSON(&encoded, plan); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	policy := decoded["policy"].(map[string]any)
	if policy["overall_timeout_ms"] != float64((45 * time.Minute).Milliseconds()) {
		t.Fatalf("JSON policy duration = %#v", policy["overall_timeout_ms"])
	}
	if policy["allow_review_web"] != true {
		t.Fatalf("JSON web authorization = %#v", policy["allow_review_web"])
	}
	reviewers := decoded["reviewers"].([]any)
	if reviewers[0].(map[string]any)["max_turns"] != float64(50) {
		t.Fatalf("JSON reviewer = %#v", reviewers[0])
	}
}

func findPlannedReviewer(t *testing.T, reviewers []planReviewer, name string) planReviewer {
	t.Helper()
	for _, reviewer := range reviewers {
		if reviewer.Name == name {
			return reviewer
		}
	}
	t.Fatalf("planned reviewer %s not found in %#v", name, reviewers)
	return planReviewer{}
}

func findPlannedCapacity(t *testing.T, capacities []planProviderCapacity, provider string) planProviderCapacity {
	t.Helper()
	for _, capacity := range capacities {
		if capacity.Provider == provider {
			return capacity
		}
	}
	t.Fatalf("planned capacity %s not found in %#v", provider, capacities)
	return planProviderCapacity{}
}

func containsSubstring(values []string, candidate string) bool {
	for _, value := range values {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
