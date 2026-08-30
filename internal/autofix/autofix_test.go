//go:build !windows

package autofix

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/orchestrator"
	"github.com/herikwebb/cora/internal/record"
)

func TestRunnerFixesAndReReviewsCompleteDiff(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, true)
	reviewer := &scriptedReviewer{t: t}
	cfg := autoFixConfig(agentPath)
	var progress strings.Builder
	loop, err := (Runner{Reviewer: reviewer, Progress: &progress}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateApproved || len(loop.Iterations) != 2 || loop.FinalDecision == nil || loop.FinalDecision.State != model.StateApproved {
		t.Fatalf("auto-fix loop = %#v", loop)
	}
	if len(reviewer.targets) != 2 || reviewer.targets[0].Mode != "branch" || reviewer.targets[1].Mode != "working-tree" || reviewer.targets[1].BaseSHA != initial.BaseSHA {
		t.Fatalf("reviewed targets = %#v", reviewer.targets)
	}
	if reviewer.options[0].AutoFixLoopID != loop.LoopID || reviewer.options[1].AutoFixIteration != 2 {
		t.Fatalf("review linkage = %#v", reviewer.options)
	}
	fix := loop.Iterations[0].Fix
	if fix == nil || fix.Status != "completed" || fix.BeforeDiffHash == fix.AfterDiffHash || !fix.Usage.TurnsKnown || !fix.Usage.APIEquivalentCostKnown {
		t.Fatalf("fix attempt = %#v", fix)
	}
	for _, name := range []string{"manifest.json", "events.jsonl", "heartbeat.json", "iterations/001/before.diff", "iterations/001/after.diff", "iterations/001/fix.prompt.md", "iterations/001/fix.policy.md", "iterations/001/agent.events.jsonl"} {
		if _, err := os.Stat(filepath.Join(loop.RecordPath, name)); err != nil {
			t.Errorf("missing auto-fix record %s: %v", name, err)
		}
	}
	if !strings.Contains(progress.String(), "iteration 1/3") || !strings.Contains(progress.String(), "consensus reached") {
		t.Fatalf("auto-fix progress:\n%s", progress.String())
	}
}

func TestRunnerStopsWhenAgentMakesNoMeaningfulChange(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, false)
	cfg := autoFixConfig(agentPath)
	loop, err := (Runner{Reviewer: &scriptedReviewer{t: t}}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "no meaningful diff change") || len(loop.Iterations) != 1 {
		t.Fatalf("no-change loop = %#v", loop)
	}
	if _, err := os.Stat(filepath.Join(loop.RecordPath, "iterations/001/after.diff")); err != nil {
		t.Fatalf("no-change attempt did not preserve its final patch: %v", err)
	}
}

func TestRunnerPreservesAgentAttemptWhenPostAgentSnapshotFails(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeIndexCorruptingAgent(t, agentPath)
	loop, err := (Runner{Reviewer: &scriptedReviewer{t: t}}).Run(context.Background(), repo, initial, autoFixConfig(agentPath))
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "capture post-fix patch") {
		t.Fatalf("post-agent snapshot failure = %#v", loop)
	}
	fix := loop.Iterations[0].Fix
	if fix == nil || fix.Status != "completed" || !fix.Usage.TurnsKnown || fix.Usage.Turns != 1 {
		t.Fatalf("returned fix attempt = %#v", fix)
	}
	var persisted model.AutoFixLoop
	if err := record.ReadJSON(filepath.Join(loop.RecordPath, "manifest.json"), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Iterations) != 1 || persisted.Iterations[0].Fix == nil || persisted.Iterations[0].Fix.Status != "completed" || persisted.Iterations[0].Fix.Usage.Turns != 1 {
		t.Fatalf("persisted fix attempt = %#v", persisted.Iterations)
	}
}

func TestRunnerStopsWhenCompletedAgentUsageIsUnavailable(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgentWithoutUsage(t, agentPath)
	cfg := autoFixConfig(agentPath)
	loop, err := (Runner{Reviewer: &scriptedReviewer{t: t}}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "turn usage is unavailable") {
		t.Fatalf("missing-usage loop = %#v", loop)
	}
	if len(loop.Iterations) != 1 || loop.Iterations[0].Fix == nil || loop.Iterations[0].Fix.Status != "completed" {
		t.Fatalf("missing-usage attempt = %#v", loop.Iterations)
	}
	if loop.Usage.TurnsKnown || !loop.Usage.TurnsPartial || loop.Usage.Turns != 2 {
		t.Fatalf("turn usage should preserve the known review subtotal as partial: %#v", loop.Usage)
	}
	if loop.Usage.APIEquivalentCostKnown || !loop.Usage.APIEquivalentCostPartial || loop.Usage.APIEquivalentCostUSD != 0.01 {
		t.Fatalf("cost usage should preserve the known review subtotal as partial: %#v", loop.Usage)
	}
}

func TestRunnerStopsBeforeRefixingEquivalentFindings(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, true)
	loop, err := (Runner{Reviewer: &scriptedReviewer{t: t, repeatFinding: true}}).Run(context.Background(), repo, initial, autoFixConfig(agentPath))
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "equivalent qualifying findings repeated") || len(loop.Iterations) != 2 || loop.Iterations[1].Fix != nil {
		t.Fatalf("repeated-finding loop = %#v", loop)
	}
}

func TestRunnerStopsFailClosedOnFailedCheck(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	cfg := autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run"))
	loop, err := (Runner{Reviewer: &scriptedReviewer{t: t, failedCheck: true}}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "validation checks did not pass") || loop.Iterations[0].Fix != nil {
		t.Fatalf("failed-check loop = %#v", loop)
	}
}

func TestRunnerRejectsApprovalWhenWorkingTreeChangesDuringReview(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	loop, err := (Runner{Reviewer: mutatingApprovalReviewer{t: t}}).Run(context.Background(), repo, initial, autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run")))
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateIncomplete || !strings.Contains(loop.Reason, "working tree changed during review") {
		t.Fatalf("stale approval loop = %#v", loop)
	}
	if loop.FinalDecision == nil || loop.FinalDecision.State != model.StateApproved {
		t.Fatalf("stale approval did not preserve the reviewer decision: %#v", loop.FinalDecision)
	}
}

func TestUnanimousApprovalRejectsAdjudicatedDisagreement(t *testing.T) {
	if unanimousApproval(map[string]string{"codex": "request_changes", "claude": "approve"}) {
		t.Fatal("disputed reviewer result was treated as unanimous")
	}
	if !unanimousApproval(map[string]string{"codex": "approve", "claude": "approve"}) {
		t.Fatal("matching approvals were not treated as unanimous")
	}
}

func TestQualifyingFindingsHonorSeverityThreshold(t *testing.T) {
	findings := []model.ConsolidatedFinding{
		{ID: "b", Severity: "blocker"}, {ID: "m", Severity: "major"},
		{ID: "n", Severity: "minor"}, {ID: "i", Severity: "note"},
	}
	if got := findingIDs(qualifyingFindings(findings, "major")); strings.Join(got, ",") != "b,m" {
		t.Fatalf("major threshold = %v", got)
	}
	if got := findingIDs(qualifyingFindings(findings, "minor")); strings.Join(got, ",") != "b,m,n" {
		t.Fatalf("minor threshold = %v", got)
	}
}

func TestAddUsageTreatsUnknownContributorAsPartial(t *testing.T) {
	usage := addUsage(knownUsage(), model.Usage{})
	if usage.TurnsKnown || !usage.TurnsPartial || usage.Turns != 2 {
		t.Fatalf("turn usage = %#v", usage)
	}
	if usage.ThinkingTokensKnown || !usage.ThinkingTokensPartial || usage.ThinkingTokens != 10 {
		t.Fatalf("thinking usage = %#v", usage)
	}
	if usage.APIEquivalentCostKnown || !usage.APIEquivalentCostPartial || usage.APIEquivalentCostUSD != 0.01 {
		t.Fatalf("cost usage = %#v", usage)
	}
}

func TestSameBranchNameRemovesOnlyRemotePrefix(t *testing.T) {
	tests := []struct {
		name    string
		branch  string
		baseRef string
		want    bool
	}{
		{name: "remote branch with slash", branch: "release/1.0", baseRef: "origin/release/1.0", want: true},
		{name: "fully qualified remote branch with slash", branch: "release/1.0", baseRef: "refs/remotes/origin/release/1.0", want: true},
		{name: "fully qualified local branch", branch: "release/1.0", baseRef: "refs/heads/release/1.0", want: true},
		{name: "fully qualified different local branch", branch: "1.0", baseRef: "refs/heads/release/1.0", want: false},
		{name: "same final component on different branch", branch: "1.0", baseRef: "origin/release/1.0", want: false},
		{name: "different nested branch", branch: "hotfix/1.0", baseRef: "origin/release/1.0", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sameBranchName(test.branch, test.baseRef); got != test.want {
				t.Fatalf("sameBranchName(%q, %q) = %t, want %t", test.branch, test.baseRef, got, test.want)
			}
		})
	}
}

func TestFixPromptIncludesAuditedFindingContextAndGitBoundary(t *testing.T) {
	prompt := fixPrompt(gitx.Repo{Root: "/repo"}, model.Target{BaseRef: "upstream/main", BaseSHA: "base", DiffHash: "diff"}, model.Decision{RunID: "run-1"}, []model.ConsolidatedFinding{{
		ID: "f1", Severity: "major", Claim: "reachable defect", Evidence: []string{"source reaches sink"}, SuggestedFixes: []string{"validate source"},
	}}, 2)
	for _, want := range []string{"run-1", "upstream/main", "source reaches sink", "validate source", "git -C \"/repo\" diff", "Do not commit", "untrusted proposals"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("fix prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

type scriptedReviewer struct {
	t             *testing.T
	targets       []model.Target
	options       []orchestrator.RunOptions
	repeatFinding bool
	failedCheck   bool
}

type mutatingApprovalReviewer struct {
	t *testing.T
}

func (m mutatingApprovalReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, _ orchestrator.RunOptions) (model.Decision, error) {
	if err := os.WriteFile(filepath.Join(repo.Root, "unreviewed.txt"), []byte("changed while reviewers were running\n"), 0o644); err != nil {
		m.t.Fatal(err)
	}
	usage := knownUsage()
	return model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: "review-stale", State: model.StateApproved,
		Reason: "all reviewers approved", BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"}, Checks: map[string]string{},
		IncrementalUsage: usage, Usage: usage,
	}, nil
}

func (s *scriptedReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, options orchestrator.RunOptions) (model.Decision, error) {
	s.targets = append(s.targets, target)
	s.options = append(s.options, options)
	usage := knownUsage()
	if len(s.targets) == 1 || s.repeatFinding {
		checks := map[string]string{}
		if s.failedCheck {
			checks["unit"] = "failed"
		}
		return model.Decision{
			SchemaVersion: model.SchemaVersion, RunID: "review-1", State: model.StateChangesRequested,
			Reason: "major finding", BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
			Reviewers: map[string]string{"codex": "request_changes", "claude": "approve"},
			Findings: []model.ConsolidatedFinding{{
				ID: "cora-bug", Severity: "major", Confidence: 0.95, File: "app.txt", Line: 2,
				Claim: "feature remains broken", Evidence: []string{"the broken marker remains"}, SuggestedFixes: []string{"replace the marker"}, Reviewers: []string{"codex"},
			}},
			Checks: checks, IncrementalUsage: usage, Usage: usage,
		}, nil
	}
	patch, err := repo.ReviewDiff(context.Background(), target)
	if err != nil {
		s.t.Fatal(err)
	}
	if !strings.Contains(string(patch), "+fixed") || target.BaseSHA != s.targets[0].BaseSHA {
		s.t.Fatalf("second review did not receive the full updated branch diff:\n%s", patch)
	}
	return model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: "review-2", State: model.StateApproved,
		Reason: "all reviewers approved", BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"}, Checks: map[string]string{},
		IncrementalUsage: usage, Usage: usage,
	}, nil
}

func autoFixTestRepo(t *testing.T) (gitx.Repo, model.Target) {
	t.Helper()
	root := t.TempDir()
	gitCommand(t, root, "init", "-b", "main")
	gitCommand(t, root, "config", "user.name", "CORA Test")
	gitCommand(t, root, "config", "user.email", "cora@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "add", "app.txt")
	gitCommand(t, root, "commit", "-m", "chore: initialize")
	gitCommand(t, root, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("base\nbroken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "add", "app.txt")
	gitCommand(t, root, "commit", "-m", "feat(app): add feature")
	repo, err := gitx.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveTarget(context.Background(), gitx.TargetOptions{Base: "main", RequireClean: true})
	if err != nil {
		t.Fatal(err)
	}
	return repo, target
}

func autoFixConfig(agentPath string) config.Config {
	cfg := config.Defaults()
	cfg.AutoFix.Command = agentPath
	cfg.AutoFix.MaxIterations = 3
	cfg.AutoFix.MaxDuration.Duration = 10 * time.Second
	cfg.AutoFix.AgentTimeout.Duration = 5 * time.Second
	cfg.AutoFix.MaxTurns = 20
	cfg.AutoFix.MaxCostUSD = 10
	return cfg
}

func knownUsage() model.Usage {
	return model.Usage{Turns: 2, TurnsKnown: true, ThinkingTokens: 10, ThinkingTokensKnown: true, APIEquivalentCostUSD: 0.01, APIEquivalentCostKnown: true, CostSource: "test"}
}

func writeAgent(t *testing.T, path string, modify bool) {
	t.Helper()
	modification := ""
	if modify {
		modification = "printf 'base\\nfixed\\n' > app.txt\n"
	}
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli test"; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then echo "Logged in using ChatGPT"; exit 0; fi
` + modification + `echo '{"type":"thread.started","model_name":"gpt-5.6-sol"}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"reasoning_tokens":10}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeIndexCorruptingAgent(t *testing.T, path string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli test"; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then echo "Logged in using ChatGPT"; exit 0; fi
printf 'corrupt-index\n' > .git/index
echo '{"type":"thread.started","model_name":"gpt-5.6-sol"}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"reasoning_tokens":10}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeAgentWithoutUsage(t *testing.T, path string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli test"; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then echo "Logged in using ChatGPT"; exit 0; fi
printf 'base\nfixed\n' > app.txt
echo '{"type":"thread.started","model_name":"gpt-5.6-sol"}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func gitCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
