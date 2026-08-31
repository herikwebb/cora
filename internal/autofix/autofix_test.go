//go:build !windows

package autofix

import (
	"context"
	"fmt"
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

func TestFormatQueueETAReportsExceededEstimateInsteadOfZero(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if got := formatQueueETA(now.Add(-time.Second), now); got != "estimate-exceeded" {
		t.Fatalf("expired queue ETA = %q", got)
	}
	if got := formatQueueETA(now.Add(500*time.Millisecond), now); got != "<1s" {
		t.Fatalf("subsecond queue ETA = %q", got)
	}
}

func TestRunAgentPreservesTypedQuotaRetryAt(t *testing.T) {
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", t.TempDir())
	retryAt := time.Now().UTC().Add(time.Hour).Round(time.Second)
	if err := record.RecordProviderQuota("codex", "session limit reached", retryAt); err != nil {
		t.Fatal(err)
	}
	cfg := autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run"))
	run := record.Run{ID: "loop", Path: t.TempDir()}
	attempt := (Runner{}).runAgent(context.Background(), gitx.Repo{Root: t.TempDir()}, run, t.TempDir(), "prompt", model.Target{DiffHash: "before"}, cfg)
	if !attempt.Retryable || attempt.FailureKind != "quota" || attempt.RetryAt == nil || !attempt.RetryAt.Equal(retryAt) {
		t.Fatalf("typed quota attempt = %#v", attempt)
	}
}

func TestRetryableQuotaIncludesOutcomeDependentDeferredReviewers(t *testing.T) {
	retryAt := time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC)
	manifest := model.Manifest{
		RunID: "quota-run",
		Reviewers: []model.ReviewerResult{
			{
				Reviewer: "codex", Status: "completed",
				Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true},
			},
			{
				Reviewer: "claude", Status: "incomplete", FailureKind: "quota",
				Retryable: true, RetryAt: &retryAt,
			},
		},
		SecurityReviews: []model.ReviewerResult{{
			Reviewer: "claude-security", Status: "deferred", FailureKind: "outcome_fixed", Retryable: true,
		}},
		Checks: []model.CheckResult{{Name: "test", Status: "passed"}},
	}
	run := record.Run{ID: manifest.RunID, Path: t.TempDir()}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	decision := model.Decision{
		RunID: manifest.RunID, RecordPath: run.Path,
		Checks: map[string]string{"test": "passed"},
	}

	gotRetryAt, reviewers, ok := retryableQuota(decision)
	if !ok || gotRetryAt == nil || !gotRetryAt.Equal(retryAt) {
		t.Fatalf("retryable quota = retry_at %v, reviewers %v, ok %t", gotRetryAt, reviewers, ok)
	}
	if got := strings.Join(reviewers, ","); got != "claude,claude-security" {
		t.Fatalf("resume reviewers = %q", got)
	}
	if !containsRetryableReviewer(manifest, map[string]bool{"claude": true, "claude-security": true}) {
		t.Fatal("quota and its outcome-dependent deferred reviewer should form a valid resume set")
	}
	if containsRetryableReviewer(manifest, map[string]bool{"claude": true}) {
		t.Fatal("resume set must not omit the outcome-dependent deferred reviewer")
	}
	if containsRetryableReviewer(manifest, map[string]bool{"claude-security": true}) {
		t.Fatal("an outcome-dependent deferral is not retryable without its quota failure")
	}
}

func TestRetryableQuotaRejectsOtherReviewerOrCheckFailures(t *testing.T) {
	retryAt := time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC)
	quota := model.ReviewerResult{
		Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Retryable: true, RetryAt: &retryAt,
	}
	tests := []struct {
		name     string
		manifest model.Manifest
		checks   map[string]string
	}{
		{
			name: "another reviewer failed",
			manifest: model.Manifest{Reviewers: []model.ReviewerResult{
				quota,
				{Reviewer: "codex", Status: "incomplete", FailureKind: "provider_error"},
			}},
		},
		{
			name:     "manifest check failed",
			manifest: model.Manifest{Reviewers: []model.ReviewerResult{quota}, Checks: []model.CheckResult{{Name: "test", Status: "failed"}}},
			checks:   map[string]string{"test": "failed"},
		},
		{
			name:     "decision check incomplete",
			manifest: model.Manifest{Reviewers: []model.ReviewerResult{quota}, Checks: []model.CheckResult{{Name: "test", Status: "passed"}}},
			checks:   map[string]string{"test": "incomplete"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.manifest.RunID = "quota-run"
			run := record.Run{ID: tt.manifest.RunID, Path: t.TempDir()}
			if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), tt.manifest); err != nil {
				t.Fatal(err)
			}
			if retryAt, reviewers, ok := retryableQuota(model.Decision{RunID: run.ID, RecordPath: run.Path, Checks: tt.checks}); ok {
				t.Fatalf("unexpected quota pause: retry_at %v reviewers %v", retryAt, reviewers)
			}
		})
	}
}

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

func TestRunnerUsesApprovedBaselineDeltaThenRequiresFullFinalReview(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, true)
	reviewer := &deltaAwareReviewer{t: t, initial: initial}
	cfg := autoFixConfig(agentPath)
	cfg.AutoFix.Threshold = "minor"
	baselineRun := writeAutoFixApprovalBaseline(t, repo, initial, cfg)

	loop, err := (Runner{Reviewer: reviewer}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateApproved || loop.BaselineRunID != baselineRun.ID || loop.BaselineDiffHash != initial.DiffHash {
		t.Fatalf("delta-aware loop = %#v", loop)
	}
	if len(loop.Iterations) != 3 {
		t.Fatalf("iterations = %#v", loop.Iterations)
	}
	if got := []string{loop.Iterations[0].ReviewScope, loop.Iterations[1].ReviewScope, loop.Iterations[2].ReviewScope}; strings.Join(got, ",") != "approved-baseline,approved-baseline-delta,full-final" {
		t.Fatalf("review scopes = %v", got)
	}
	if loop.Iterations[1].ApprovalBaselineRunID != baselineRun.ID || loop.Iterations[1].FullDiffHash == loop.Iterations[1].ReviewDiffHash {
		t.Fatalf("delta lineage = %#v", loop.Iterations[1])
	}
	if reviewer.deltaCalls != 1 || reviewer.fullCalls != 1 {
		t.Fatalf("review calls: delta=%d full=%d", reviewer.deltaCalls, reviewer.fullCalls)
	}
}

func TestRunnerRejectsBaselineApprovedUnderWeakerReviewPolicy(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, true)
	oldPolicy := autoFixConfig(agentPath)
	baselineRun := writeAutoFixApprovalBaseline(t, repo, initial, oldPolicy)
	reviewer := &scriptedReviewer{t: t}
	cfg := oldPolicy
	cfg.StrictPolicy = true
	cfg.Escalation.ForceSecuritySensitive = true
	cfg.Checks = []config.Check{{
		Name: "go-test", Command: []string{"go", "test", "./..."}, Timeout: config.Duration{Duration: time.Minute}, Profile: "go",
	}}

	loop, err := (Runner{Reviewer: reviewer}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StateApproved {
		t.Fatalf("auto-fix loop = %#v", loop)
	}
	if loop.BaselineRunID != "" || len(loop.Iterations) < 1 || loop.Iterations[0].ReviewScope != "full" || loop.Iterations[0].ReviewRunID == baselineRun.ID {
		t.Fatalf("weaker approval was reused as a baseline: %#v", loop)
	}
	if len(reviewer.targets) != 2 || reviewer.targets[0].DiffHash != initial.DiffHash {
		t.Fatalf("expected a fresh initial full review, got %#v", reviewer.targets)
	}
}

func TestRunnerRejectsBaselineFromDifferentRepositoryIdentity(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeAgent(t, agentPath, true)
	cfg := autoFixConfig(agentPath)
	baselineRun := writeAutoFixApprovalBaseline(t, repo, initial, cfg)
	manifest, err := record.LoadManifest(baselineRun)
	if err != nil {
		t.Fatal(err)
	}
	manifest.RepositoryIdentity = "github.com/example/different-repository"
	if err := record.WriteJSON(filepath.Join(baselineRun.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	reviewer := &scriptedReviewer{t: t}

	loop, err := (Runner{Reviewer: reviewer}).Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loop.BaselineRunID != "" || len(loop.Iterations) == 0 || loop.Iterations[0].ReviewScope != "full" {
		t.Fatalf("foreign-repository approval was reused: %#v", loop)
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

func TestRunnerPausesRetryableQuotaWithoutFinishingLoop(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	retryAt := time.Now().UTC().Add(time.Hour).Round(time.Second)
	reviewer := quotaPauseReviewer{t: t, retryAt: retryAt}
	loop, err := (Runner{Reviewer: reviewer}).Run(context.Background(), repo, initial, autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run")))
	if err != nil {
		t.Fatal(err)
	}
	if loop.State != model.StatePaused || loop.PausedAt == nil || loop.RetryAt == nil || !loop.RetryAt.Equal(retryAt) || loop.FinishedAt != nil {
		t.Fatalf("paused loop = %#v", loop)
	}
	if loop.ResumePhase != "review" || loop.ResumeReviewRunID == "" || strings.Join(loop.ResumeReviewers, ",") != "claude" {
		t.Fatalf("resume metadata = %#v", loop)
	}
	persistedRun, err := record.New(repo.CommonDir).ResolveAutoFix(loop.LoopID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := record.LoadAutoFixLoop(persistedRun)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != model.StatePaused || persisted.FinishedAt != nil || persisted.RetryAt == nil || !persisted.RetryAt.Equal(retryAt) {
		t.Fatalf("persisted pause = %#v", persisted)
	}
	contents, err := os.ReadFile(filepath.Join(loop.RecordPath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), `"finished_at"`) {
		t.Fatalf("paused loop was serialized as terminal:\n%s", contents)
	}
}

func TestRunnerResumeRetriesOnlyQuotaReviewerInSameParentLoop(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	reviewer := &resumableQuotaReviewer{t: t, retryAt: time.Now().UTC().Add(-time.Second)}
	runner := Runner{Reviewer: reviewer}
	cfg := autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run"))
	paused, err := runner.Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != model.StatePaused {
		t.Fatalf("initial loop = %#v", paused)
	}
	resumed, err := runner.Resume(context.Background(), repo, paused.LoopID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.LoopID != paused.LoopID || resumed.RecordPath != paused.RecordPath || resumed.State != model.StateApproved || resumed.ResumeCount != 1 {
		t.Fatalf("resumed loop = %#v", resumed)
	}
	if len(resumed.Iterations) != 1 || len(resumed.Iterations[0].ReviewAttemptRunIDs) != 2 || resumed.Iterations[0].ReviewAttemptRunIDs[0] == resumed.Iterations[0].ReviewAttemptRunIDs[1] {
		t.Fatalf("review retry lineage = %#v", resumed.Iterations)
	}
	if reviewer.calls != 2 || !reviewer.retriedOnlyClaude {
		t.Fatalf("reviewer calls=%d retriedOnlyClaude=%t", reviewer.calls, reviewer.retriedOnlyClaude)
	}
}

func TestRunnerResumeRejectsWorkingTreeDrift(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	reviewer := &resumableQuotaReviewer{t: t, retryAt: time.Now().UTC().Add(-time.Second)}
	runner := Runner{Reviewer: reviewer}
	cfg := autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run"))
	paused, err := runner.Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), []byte("base\nunreviewed drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Resume(context.Background(), repo, paused.LoopID, cfg); err == nil || !strings.Contains(err.Error(), "working tree changed while auto-fix was paused") {
		t.Fatalf("resume drift error = %v", err)
	}
	if reviewer.calls != 1 {
		t.Fatalf("provider was invoked after resume drift: calls=%d", reviewer.calls)
	}
}

func TestRunnerResumeRejectsCorruptChildReviewLineage(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*model.Manifest)
		wantErr string
	}{
		{
			name: "missing loop identity",
			mutate: func(manifest *model.Manifest) {
				manifest.AutoFixLoopID = ""
			},
			wantErr: "different auto-fix loop",
		},
		{
			name: "missing iteration identity",
			mutate: func(manifest *model.Manifest) {
				manifest.AutoFixIteration = 0
			},
			wantErr: "iteration does not match",
		},
		{
			name: "full target head mismatch",
			mutate: func(manifest *model.Manifest) {
				fullTarget := manifest.Target
				fullTarget.HeadSHA = "different-head"
				manifest.FullTarget = &fullTarget
			},
			wantErr: "full-target lineage does not match",
		},
		{
			name: "child repository identity mismatch",
			mutate: func(manifest *model.Manifest) {
				manifest.RepositoryIdentity = "github.com/example/different-repository"
			},
			wantErr: "different repository identity",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, initial := autoFixTestRepo(t)
			reviewer := &resumableQuotaReviewer{t: t, retryAt: time.Now().UTC().Add(-time.Second)}
			runner := Runner{Reviewer: reviewer}
			cfg := autoFixConfig(filepath.Join(t.TempDir(), "agent-must-not-run"))
			paused, err := runner.Run(context.Background(), repo, initial, cfg)
			if err != nil {
				t.Fatal(err)
			}
			reviewRun, err := record.New(repo.CommonDir).Resolve(paused.ResumeReviewRunID)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := record.LoadManifest(reviewRun)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(&manifest)
			if err := record.WriteJSON(filepath.Join(reviewRun.Path, "manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}

			if _, err := runner.Resume(context.Background(), repo, paused.LoopID, cfg); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resume lineage error = %v", err)
			}
			if reviewer.calls != 1 {
				t.Fatalf("provider was invoked after corrupt lineage: calls=%d", reviewer.calls)
			}
		})
	}
}

func TestRunnerResumeRetriesQuotaFailedCodingAgentAndPreservesAttempts(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeQuotaThenFixAgent(t, agentPath, filepath.Join(t.TempDir(), "attempted"))
	reviewer := &persistentFindingReviewer{t: t}
	runner := Runner{Reviewer: reviewer}
	cfg := autoFixConfig(agentPath)
	paused, err := runner.Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != model.StatePaused || paused.ResumePhase != "fix" || len(paused.Iterations) != 1 {
		t.Fatalf("coding-agent pause = %#v", paused)
	}
	resumed, err := runner.Resume(context.Background(), repo, paused.LoopID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != model.StateApproved || len(resumed.Iterations) != 2 || len(resumed.Iterations[0].FixAttempts) != 2 {
		t.Fatalf("coding-agent resume = %#v", resumed)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		path := filepath.Join(resumed.RecordPath, "iterations", "001", "fix-attempts", fmt.Sprintf("%03d", attempt), "after.diff")
		if attempt == 1 {
			// The original attempt predates per-retry subdirectories and remains
			// preserved by the legacy iterations/001/after.diff path.
			path = filepath.Join(resumed.RecordPath, "iterations", "001", "after.diff")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing coding-agent attempt patch %s: %v", path, err)
		}
	}
}

func TestRunnerFixPhaseResumeRestoresImmutableReviewPolicy(t *testing.T) {
	repo, initial := autoFixTestRepo(t)
	agentPath := filepath.Join(t.TempDir(), "codex")
	writeQuotaThenFixAgent(t, agentPath, filepath.Join(t.TempDir(), "attempted"))
	reviewer := &persistentFindingReviewer{t: t}
	runner := Runner{Reviewer: reviewer}
	cfg := autoFixConfig(agentPath)
	cfg.StrictPolicy = true
	cfg.Escalation.ForceSecuritySensitive = true
	cfg.Escalation.AdjudicateDisagreements = true
	cfg.Checks = []config.Check{{
		Name: "go-test", Command: []string{"go", "test", "./..."}, Timeout: config.Duration{Duration: 2 * time.Minute},
		EnvAllowlist: []string{"GOCACHE"}, Profile: "go",
	}}

	paused, err := runner.Run(context.Background(), repo, initial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != model.StatePaused || paused.ResumePhase != "fix" || paused.ReviewPolicy == nil {
		t.Fatalf("coding-agent pause policy = %#v", paused)
	}
	weaker := autoFixConfig(filepath.Join(t.TempDir(), "wrong-agent"))
	weaker.StrictPolicy = false
	weaker.Escalation.ForceSecuritySensitive = false
	weaker.Escalation.AdjudicateDisagreements = false
	weaker.Checks = nil
	resumed, err := runner.Resume(context.Background(), repo, paused.LoopID, weaker)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != model.StateApproved {
		t.Fatalf("resumed loop = %#v", resumed)
	}
	if len(reviewer.configs) != 2 {
		t.Fatalf("review calls = %d", len(reviewer.configs))
	}
	restored := reviewer.configs[1]
	if !restored.StrictPolicy || !restored.Escalation.ForceSecuritySensitive || !restored.Escalation.AdjudicateDisagreements {
		t.Fatalf("restored policy flags = strict:%t security:%t adjudicate:%t", restored.StrictPolicy, restored.Escalation.ForceSecuritySensitive, restored.Escalation.AdjudicateDisagreements)
	}
	if len(restored.Checks) != 1 || restored.Checks[0].Name != "go-test" || restored.Checks[0].Profile != "go" || strings.Join(restored.Checks[0].Command, " ") != "go test ./..." {
		t.Fatalf("restored effective checks = %#v", restored.Checks)
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

func TestPreserveRetryReviewerSettingsKeepsTargetedSecurityModelAndEffort(t *testing.T) {
	cfg := config.Defaults()
	cfg.Escalation.Model = "different-model"
	cfg.Escalation.Effort = "low"
	preserveRetryReviewerSettings(&cfg, model.Manifest{SecurityReviews: []model.ReviewerResult{{
		Reviewer: "claude-security", Model: "fable", Effort: "high", EscalationCause: "security_sensitive",
	}}}, map[string]bool{"claude-security": true})
	if cfg.Escalation.Model != "fable" || cfg.Escalation.Effort != "high" || !cfg.Escalation.ForceSecuritySensitive || !cfg.Reviewers.Claude.Enabled {
		t.Fatalf("preserved security settings = %#v / %#v", cfg.Escalation, cfg.Reviewers.Claude)
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

type deltaAwareReviewer struct {
	t          *testing.T
	initial    model.Target
	deltaCalls int
	fullCalls  int
}

func (r *deltaAwareReviewer) RunAutoFixReview(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, _ orchestrator.RunOptions, reviewContext model.AutoFixReviewContext) (model.Decision, error) {
	if reviewContext.ReviewScope == "full-final" {
		r.fullCalls++
		if target.BaseSHA != r.initial.BaseSHA || target.DiffHash == r.initial.DiffHash || reviewContext.FullTarget.DiffHash != target.DiffHash || reviewContext.ApprovalBaselineRunID == "" {
			r.t.Fatalf("final full target/context = %#v / %#v", target, reviewContext)
		}
		patch, err := repo.ReviewDiff(context.Background(), target)
		if err != nil {
			return model.Decision{}, err
		}
		if !strings.Contains(string(patch), "+fixed") {
			r.t.Fatalf("final review did not receive the complete updated diff:\n%s", patch)
		}
		return approvedTestDecision("final-full-review", target), nil
	}
	r.deltaCalls++
	if reviewContext.ReviewScope != "approved-baseline-delta" || reviewContext.TrustedBaseSHA != r.initial.BaseSHA || reviewContext.FullTarget.BaseSHA != r.initial.BaseSHA {
		r.t.Fatalf("delta review context = %#v", reviewContext)
	}
	if target.BaseSHA != r.initial.HeadSHA || target.HeadSHA != r.initial.HeadSHA || target.DiffHash == reviewContext.FullTarget.DiffHash {
		r.t.Fatalf("delta target = %#v; full target = %#v", target, reviewContext.FullTarget)
	}
	patch, err := repo.ReviewDiff(context.Background(), target)
	if err != nil {
		return model.Decision{}, err
	}
	if !strings.Contains(string(patch), "+fixed") || strings.Contains(string(patch), "+broken") {
		r.t.Fatalf("approved-baseline delta unexpectedly contains the original branch diff:\n%s", patch)
	}
	return approvedTestDecision("delta-review", target), nil
}

func (r *deltaAwareReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, _ orchestrator.RunOptions) (model.Decision, error) {
	r.t.Fatalf("unexpected unscoped review for target %#v", target)
	return model.Decision{}, nil
}

type quotaPauseReviewer struct {
	t       *testing.T
	retryAt time.Time
}

type resumableQuotaReviewer struct {
	t                 *testing.T
	retryAt           time.Time
	calls             int
	retriedOnlyClaude bool
}

type persistentFindingReviewer struct {
	t       *testing.T
	calls   int
	configs []config.Config
}

func (r *persistentFindingReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, cfg config.Config, options orchestrator.RunOptions) (model.Decision, error) {
	r.calls++
	r.configs = append(r.configs, cfg)
	decision := approvedTestDecision(fmt.Sprintf("review-%d", r.calls), target)
	if r.calls == 1 {
		decision.State = model.StateChangesRequested
		decision.Reason = "major finding"
		decision.Reviewers = map[string]string{"codex": "request_changes", "claude": "approve"}
		decision.Findings = []model.ConsolidatedFinding{{
			ID: "cora-bug", Severity: "major", File: "app.txt", Line: 2, Claim: "feature remains broken",
			Evidence: []string{"the broken marker remains"}, SuggestedFixes: []string{"replace the marker"}, Reviewers: []string{"codex"},
		}}
	}
	store := record.New(repo.CommonDir)
	run, err := store.Create(time.Now().UTC(), target.HeadSHA)
	if err != nil {
		return model.Decision{}, err
	}
	decision.RunID = run.ID
	decision.RecordPath = run.Path
	repositoryIdentity, err := repo.StableIdentity(context.Background())
	if err != nil {
		return model.Decision{}, err
	}
	manifest := model.Manifest{RunID: run.ID, Target: target, RepositoryIdentity: repositoryIdentity, AutoFixLoopID: options.AutoFixLoopID, AutoFixIteration: options.AutoFixIteration}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		return model.Decision{}, err
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		return model.Decision{}, err
	}
	return decision, nil
}

func (r *resumableQuotaReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, options orchestrator.RunOptions) (model.Decision, error) {
	r.calls++
	store := record.New(repo.CommonDir)
	run, err := store.Create(time.Now().UTC(), target.HeadSHA)
	if err != nil {
		return model.Decision{}, err
	}
	usage := knownUsage()
	report := func() *model.ReviewReport { return &model.ReviewReport{Verdict: "approve", ContextComplete: true} }
	reviewers := []model.ReviewerResult{{Reviewer: "codex", Status: "completed", Report: report(), Usage: usage}}
	decisionReviewers := map[string]string{"codex": "approve"}
	state := model.StateIncomplete
	reason := "Claude quota exhausted"
	if r.calls == 1 {
		reviewers = append(reviewers, model.ReviewerResult{Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Retryable: true, RetryAt: &r.retryAt, Error: reason, Usage: knownZeroUsage("quota gate")})
		decisionReviewers["claude"] = "incomplete"
	} else {
		r.retriedOnlyClaude = len(options.RetryReviewers) == 1 && options.RetryReviewers["claude"] && options.ParentRunID != "" && len(options.ReuseReviewers) == 2
		reviewers = append(reviewers, model.ReviewerResult{Reviewer: "claude", Status: "completed", Report: report(), Usage: usage})
		decisionReviewers["claude"] = "approve"
		state = model.StateApproved
		reason = "all reviewers approved"
	}
	repositoryIdentity, err := repo.StableIdentity(context.Background())
	if err != nil {
		return model.Decision{}, err
	}
	manifest := model.Manifest{
		RunID: run.ID, Target: target, RepositoryIdentity: repositoryIdentity, Reviewers: reviewers,
		ParentRunID: options.ParentRunID, AutoFixLoopID: options.AutoFixLoopID, AutoFixIteration: options.AutoFixIteration,
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		return model.Decision{}, err
	}
	decision := model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: run.ID, State: state, Reason: reason,
		BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: decisionReviewers, Checks: map[string]string{}, IncrementalUsage: usage, Usage: usage, RecordPath: run.Path,
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		return model.Decision{}, err
	}
	return decision, nil
}

func (r quotaPauseReviewer) RunWithOptions(_ context.Context, repo gitx.Repo, target model.Target, _ config.Config, _ orchestrator.RunOptions) (model.Decision, error) {
	store := record.New(repo.CommonDir)
	run, err := store.Create(time.Now().UTC(), target.HeadSHA)
	if err != nil {
		return model.Decision{}, err
	}
	result := model.ReviewerResult{
		Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Retryable: true, RetryAt: &r.retryAt,
		Error: "session quota exhausted", Usage: knownZeroUsage("provider not invoked"),
	}
	repositoryIdentity, err := repo.StableIdentity(context.Background())
	if err != nil {
		return model.Decision{}, err
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{RunID: run.ID, Target: target, RepositoryIdentity: repositoryIdentity, Reviewers: []model.ReviewerResult{result}}); err != nil {
		return model.Decision{}, err
	}
	decision := model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: run.ID, State: model.StateIncomplete,
		Reason: "one or more reviewers did not complete", BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"claude": "incomplete"}, IncrementalUsage: knownUsage(), RecordPath: run.Path,
	}
	return decision, nil
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

func writeAutoFixApprovalBaseline(t *testing.T, repo gitx.Repo, target model.Target, cfg config.Config) record.Run {
	t.Helper()
	store := record.New(repo.CommonDir)
	run, err := store.Create(time.Now().UTC().Add(-time.Second), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := repo.ReviewDiff(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	finding := model.ConsolidatedFinding{
		ID: "approved-minor", Severity: "minor", Confidence: 0.9, File: "app.txt", Line: 2,
		Claim: "the marker should be clearer", Evidence: []string{"the branch writes broken"}, SuggestedFixes: []string{"write fixed"}, Reviewers: []string{"codex"},
	}
	report := func() *model.ReviewReport { return &model.ReviewReport{Verdict: "approve", ContextComplete: true} }
	decision := model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: run.ID, State: model.StateApproved, OutcomeQualifier: "non_blocking_findings",
		BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"}, Findings: []model.ConsolidatedFinding{finding}, Checks: map[string]string{}, RecordPath: run.Path,
	}
	if err := record.WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	policy := snapshotReviewPolicy(cfg)
	repositoryIdentity, err := repo.StableIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{
		RunID: run.ID, Target: target, ReviewPolicy: &policy, RepositoryIdentity: repositoryIdentity,
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Model: cfg.Reviewers.Codex.Model, Effort: cfg.Reviewers.Codex.Effort, Report: report()},
			{Reviewer: "claude", Status: "completed", Model: cfg.Reviewers.Claude.Model, Effort: cfg.Reviewers.Claude.Effort, Report: report()},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func approvedTestDecision(runID string, target model.Target) model.Decision {
	usage := knownUsage()
	return model.Decision{
		SchemaVersion: model.SchemaVersion, RunID: runID, State: model.StateApproved,
		Reason: "all reviewers approved", BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"}, Checks: map[string]string{},
		IncrementalUsage: usage, Usage: usage,
	}
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

func writeQuotaThenFixAgent(t *testing.T, path, marker string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli test"; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then echo "Logged in using ChatGPT"; exit 0; fi
if [ ! -f "` + marker + `" ]; then
  touch "` + marker + `"
  echo '{"type":"thread.started","model_name":"gpt-5.6-sol"}'
  echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"reasoning_tokens":10}}'
  echo '{"type":"error","message":"You have hit your usage limit"}'
  exit 1
fi
printf 'base\nfixed\n' > app.txt
echo '{"type":"thread.started","model_name":"gpt-5.6-sol"}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"reasoning_tokens":10}}'
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
