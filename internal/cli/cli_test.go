package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"github.com/herikwebb/cora/internal/record"
)

func TestCommandErrorExitCodePrioritizesCallerCancellation(t *testing.T) {
	var stderr bytes.Buffer
	err := errors.Join(stateError{state: model.StateIncomplete}, context.Canceled)
	if got := commandErrorExitCode(err, &stderr); got != 130 {
		t.Fatalf("exit code = %d, want 130", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no diagnostic for caller cancellation", stderr.String())
	}
}

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

func TestReviewCommandExposesBoundedAutoFixFlags(t *testing.T) {
	command := newReviewCommand(&options{})
	for _, name := range []string{"auto-fix", "resume", "until", "max-iterations", "max-duration", "max-turns", "max-cost-usd", "agent-timeout", "allow-review-web", "web-evidence"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("review command is missing --%s", name)
		}
	}
}

func TestRetryCommandExposesAuditedLimitOverrides(t *testing.T) {
	command := newRetryCommand(&options{})
	for _, name := range []string{"reviewer-timeout", "overall-timeout", "max-turns"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("retry command is missing --%s", name)
		}
	}
}

func TestAutoFixOnlyFlagsRequireExplicitOptIn(t *testing.T) {
	command := newReviewCommand(&options{})
	command.SetArgs([]string{"--until", "minor"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "requires --auto-fix") {
		t.Fatalf("auto-fix opt-in error = %v", err)
	}
	command = newReviewCommand(&options{})
	command.SetArgs([]string{"--auto-fix", "--max-iterations", "0"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("auto-fix limit error = %v", err)
	}
	command = newReviewCommand(&options{})
	command.SetArgs([]string{"--resume", "loop-id"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "requires --auto-fix") {
		t.Fatalf("auto-fix resume opt-in error = %v", err)
	}
	command = newReviewCommand(&options{})
	command.SetArgs([]string{"--auto-fix", "--resume", "loop-id", "--strict"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "cannot change the recorded review policy") {
		t.Fatalf("auto-fix resume policy error = %v", err)
	}
	command = newReviewCommand(&options{})
	command.SetArgs([]string{"--auto-fix", "--web-evidence", "https://docs.example.com/"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be combined with --auto-fix") {
		t.Fatalf("auto-fix web evidence error = %v", err)
	}
	command = newReviewCommand(&options{})
	command.SetArgs([]string{"--auto-fix", "--resume", "loop-id", "--allow-review-web"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "cannot change the recorded review policy") {
		t.Fatalf("auto-fix resume web-policy error = %v", err)
	}
}

func TestPrintAutoFixLoopShowsStopReasonAndUsage(t *testing.T) {
	var output bytes.Buffer
	printAutoFixLoop(&output, model.AutoFixLoop{
		LoopID: "loop-1", State: model.StateIncomplete, Reason: "equivalent findings repeated",
		Threshold: "minor", MaxIterations: 5, FinalDiffHash: "1234567890", Elapsed: model.NewDuration(time.Minute),
		Usage: model.Usage{Turns: 4, TurnsKnown: true, APIEquivalentCostUSD: 1.25, APIEquivalentCostKnown: true},
		Iterations: []model.AutoFixIteration{{
			Number: 1, ReviewRunID: "run-1", ReviewState: model.StateChangesRequested,
			BlockingFindings: 1, NonBlockingFindings: 2, QualifyingFindingIDs: []string{"f1"},
		}},
		RecordPath: "/tmp/loop-1",
	})
	text := output.String()
	for _, want := range []string{"INCOMPLETE AUTO-FIX loop-1", "equivalent findings repeated", "Iterations: 1/5", "findings: blocking=1, non-blocking=2 qualifying=1", "provider-turns=4", "$1.2500", "run-1", "/tmp/loop-1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("auto-fix output does not contain %q:\n%s", want, text)
		}
	}
}

func TestPrintAutoFixLoopRecoversLegacyIterationFindingCounts(t *testing.T) {
	firstPath := t.TempDir()
	secondPath := t.TempDir()
	if err := record.WriteJSON(filepath.Join(firstPath, "decision.json"), model.Decision{
		RunID: "run-1", OpenFindings: map[string]int{"major": 1, "minor": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(secondPath, "decision.json"), model.Decision{
		RunID: "run-2", OpenFindings: map[string]int{"note": 3},
	}); err != nil {
		t.Fatal(err)
	}
	loop := model.AutoFixLoop{
		LoopID: "legacy-loop", State: model.StateIncomplete, MaxIterations: 3,
		Iterations: []model.AutoFixIteration{
			{Number: 1, ReviewRunID: "run-1", ReviewRecordPath: firstPath, ReviewState: model.StateChangesRequested, QualifyingFindingIDs: []string{"f-1"}},
			{Number: 2, ReviewRunID: "run-2", ReviewRecordPath: secondPath, ReviewState: model.StateApproved},
			{Number: 3, ReviewRunID: "missing", ReviewRecordPath: filepath.Join(t.TempDir(), "missing"), ReviewState: model.StateIncomplete, QualifyingFindingIDs: []string{"f-2"}},
		},
	}
	var output bytes.Buffer
	printAutoFixLoop(&output, loop)
	for _, want := range []string{
		"iteration 1: review=changes_requested findings: blocking=1, non-blocking=2 qualifying=1",
		"iteration 2: review=approved findings: blocking=0, non-blocking=3 qualifying=0",
		"iteration 3: review=incomplete findings: unknown qualifying=1",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("legacy loop output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestFormatDecisionFindingSummaryShowsApprovedNonBlockingFindings(t *testing.T) {
	decision := model.Decision{
		State:               model.StateApproved,
		BlockingFindings:    0,
		NonBlockingFindings: 5,
		OpenFindings:        map[string]int{"blocker": 0, "major": 0, "minor": 3, "note": 2},
		OutcomeQualifier:    "non_blocking_findings",
	}
	got := formatDecisionFindingSummary(decision)
	want := "blocking=0, non-blocking=5 (blocker=0 major=0 minor=3 note=2)"
	if got != want {
		t.Fatalf("finding summary = %q, want %q", got, want)
	}
}

func TestDecisionFindingCountsSupportsHistoricalStrictRuns(t *testing.T) {
	decision := model.Decision{
		StrictPolicy: true,
		OpenFindings: map[string]int{"blocker": 0, "major": 0, "minor": 2, "note": 1},
	}
	blocking, nonBlocking := decisionFindingCounts(decision)
	if blocking != 2 || nonBlocking != 1 {
		t.Fatalf("historical strict counts = blocking=%d non-blocking=%d; want 2, 1", blocking, nonBlocking)
	}
}

func TestPrintAutoFixLoopShowsResumeCommandWhenPaused(t *testing.T) {
	var output bytes.Buffer
	retryAt := time.Date(2026, 8, 31, 16, 0, 0, 0, time.Local)
	printAutoFixLoop(&output, model.AutoFixLoop{
		LoopID: "loop-paused", State: model.StatePaused, Reason: "provider quota",
		RetryAt: &retryAt, MaxIterations: 5,
	})
	text := output.String()
	if !strings.Contains(text, "cora review --auto-fix --resume loop-paused") || !strings.Contains(text, "Retry after:") {
		t.Fatalf("paused auto-fix output:\n%s", text)
	}
}

func TestPrintActiveRunsShowsConcurrentReviewerElapsedTime(t *testing.T) {
	var output bytes.Buffer
	printActiveRuns(&output, []model.RunSummary{
		{RunID: "run-one", HeadSHA: "aaaaaaaaaa", ElapsedMS: 65_000, ActiveExecutionMS: 35_000, ActiveTimingBasis: "sampled-awake-while-executing", Phase: "reviewers", Reviewers: map[string]string{"codex": "completed", "claude": "running"}, ReviewerVerdicts: map[string]string{"codex": "approve"}, ReviewerElapsedMS: map[string]int64{"claude": 42_000}},
		{RunID: "run-two", HeadSHA: "bbbbbbbbbb", ElapsedMS: 30_000, Phase: "reviewers", Reviewers: map[string]string{"claude": "queued"}, Queues: map[string]model.ProviderQueueStatus{"claude": {Position: 2}}},
	})
	text := output.String()
	for _, want := range []string{"run-one", "run-two", "35s", "claude=running(wall=42s)", "codex=completed(verdict=approve)", "claude=queued#2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("active output does not contain %q:\n%s", want, text)
		}
	}
}

func TestActiveReviewerSummaryUsesRawRunningStateBeforeVerdictDecoration(t *testing.T) {
	summary := activeReviewerSummary(model.RunSummary{
		Reviewers:         map[string]string{"claude": "running"},
		ReviewerVerdicts:  map[string]string{"claude": "approve"},
		ReviewerElapsedMS: map[string]int64{"claude": 42_000},
	})
	if !strings.Contains(summary, "claude=running(verdict=approve)(wall=42s)") {
		t.Fatalf("verdict decoration hid running elapsed time: %q", summary)
	}
}

func TestActiveReviewerSummaryShowsHolderAfterExpiredQueueETA(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	timeoutAt := time.Now().Add(2 * time.Minute)
	summary := activeReviewerSummary(model.RunSummary{
		Reviewers: map[string]string{"claude": "queued"},
		Queues: map[string]model.ProviderQueueStatus{"claude": {
			Position: 1, ETAAt: &deadline,
			Holders: []model.ProviderCapacityHolder{{RunID: "run-active", Reviewer: "claude", TimeoutAt: &timeoutAt}},
		}},
	})
	for _, want := range []string{"holder=claude@run-active", "timeout_in="} {
		if !strings.Contains(summary, want) {
			t.Fatalf("capacity-holder summary %q does not contain %q", summary, want)
		}
	}
	if strings.Contains(summary, "estimate-exceeded") {
		t.Fatalf("expired historical estimate remained in summary: %q", summary)
	}
}

func TestPrintConsolidatedDetailsIncludesEvidenceFixesAndRisks(t *testing.T) {
	var output bytes.Buffer
	printConsolidatedDetails(&output, model.Decision{
		Findings: []model.ConsolidatedFinding{{
			Severity: "minor", Confidence: 0.8, File: "app.go", Line: 12, Claim: "leak",
			Evidence: []string{"handle is never closed"}, SuggestedFixes: []string{"defer handle.Close()"}, Reviewers: []string{"codex"},
			CarriedFromRunIDs: []string{"run-prior"},
		}},
		ResidualRisks: []string{"integration test not run"},
	})
	text := output.String()
	for _, want := range []string{"Confidence: 80%", "Carried from runs: run-prior", "Evidence: handle is never closed", "Suggested fix: defer handle.Close()", "integration test not run"} {
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
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude-security", Status: "incomplete", FailureKind: "quota"},
	}
	selected, err := selectRetryReviewers(results, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || !selected["claude-security"] || selected["codex"] || selected["claude"] {
		t.Fatalf("selected reviewers = %#v", selected)
	}
}

func TestAllRetryReviewersIncludesConditionalRoles(t *testing.T) {
	selected := allRetryReviewers()
	for _, reviewer := range []string{"codex", "claude", "claude-security", "claude-escalation", "claude-cross-examination"} {
		if !selected[reviewer] {
			t.Fatalf("expected web-backed retry to select %q: %#v", reviewer, selected)
		}
	}
	if len(selected) != 5 {
		t.Fatalf("unexpected web-backed retry selection: %#v", selected)
	}
}

func TestSelectRetryReviewersForWebRunRejectsTargetedRetry(t *testing.T) {
	selected, wholeReview, err := selectRetryReviewersForRun(true, nil, []string{"codex"})
	if err == nil || !strings.Contains(err.Error(), "omit --reviewer") {
		t.Fatalf("targeted web retry error = %v", err)
	}
	if selected != nil || wholeReview {
		t.Fatalf("rejected web retry returned selection %#v, whole=%t", selected, wholeReview)
	}

	selected, wholeReview, err = selectRetryReviewersForRun(true, nil, nil)
	if err != nil || !wholeReview || len(selected) != 5 {
		t.Fatalf("whole web retry = %#v, whole=%t, err=%v", selected, wholeReview, err)
	}
}

func TestSelectRetryReviewersDefaultsToIncompleteOrAbstainingCompletedReports(t *testing.T) {
	tests := []struct {
		name   string
		report *model.ReviewReport
	}{
		{
			name: "context incomplete approval",
			report: &model.ReviewReport{
				Verdict: "approve", ContextComplete: false,
			},
		},
		{
			name: "reserved finalizer abstention",
			report: &model.ReviewReport{
				Verdict: "abstain", ContextComplete: false,
			},
		},
		{
			name: "context complete abstention",
			report: &model.ReviewReport{
				Verdict: "abstain", ContextComplete: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := selectRetryReviewers([]model.ReviewerResult{{
				Reviewer: "claude", Status: "completed", Report: test.report,
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(selected) != 1 || !selected["claude"] {
				t.Fatalf("selected reviewers = %#v", selected)
			}
		})
	}
}

func TestPrepareRetryResultsUsesLatestAttemptOnlyForSelectedReviewer(t *testing.T) {
	completed := &model.ReviewReport{Verdict: "approve"}
	preserved := []model.ReviewerResult{
		{Reviewer: "claude", Status: "completed", Report: completed, Attempt: 1, ReusedFromRunID: "first"},
		{Reviewer: "codex", Status: "completed", Report: completed, Attempt: 1, ReusedFromRunID: "first"},
	}
	latest := []model.ReviewerResult{
		{Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Attempt: 3},
		{Reviewer: "codex", Status: "incomplete", Attempt: 2},
	}

	results := prepareRetryResults(preserved, latest, map[string]bool{"claude": true})
	if len(results) != 2 || results[0].Reviewer != "claude" || results[0].Attempt != 3 || results[0].Report != nil || results[0].Status != "incomplete" {
		t.Fatalf("selected retry result = %#v", results)
	}
	if results[1].Reviewer != "codex" || results[1].Status != "completed" || results[1].Report == nil || results[1].ReusedFromRunID != "first" {
		t.Fatalf("preserved retry result = %#v", results)
	}
}

func TestRetrySelectionUsesLatestIncompleteAttemptEvenWhenApprovalIsPreserved(t *testing.T) {
	completed := model.ReviewerResult{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve"}}
	latest := completed
	latest.Status = "incomplete"
	latest.Report = nil
	latest.FailureKind = "quota"

	selected, err := selectRetryReviewers([]model.ReviewerResult{latest}, nil)
	if err != nil || !selected["claude"] {
		t.Fatalf("latest failed attempt was not selected for retry: %#v, %v", selected, err)
	}
}

func TestRetryAutoFixReviewContextRestoresTrustedBaselineLineage(t *testing.T) {
	store := record.New(t.TempDir())
	patch := []byte("diff --git a/app.go b/app.go\n")
	sum := sha256.Sum256(patch)
	baselineTarget := model.Target{BaseSHA: "trusted", HeadSHA: "approved", DiffHash: hex.EncodeToString(sum[:]), Finalizable: true}
	run, err := store.Create(time.Unix(1, 0), baselineTarget.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	report := &model.ReviewReport{Verdict: "approve", ContextComplete: true}
	if err := record.WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{
		RunID: run.ID, Target: baselineTarget, ReviewScope: "full",
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Report: report},
			{Reviewer: "claude", Status: "completed", Report: report},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), model.Decision{
		RunID: run.ID, State: model.StateApproved, BaseSHA: baselineTarget.BaseSHA,
		HeadSHA: baselineTarget.HeadSHA, DiffHash: baselineTarget.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"},
		Findings:  []model.ConsolidatedFinding{{ID: "known", Claim: "baseline evidence"}},
	}); err != nil {
		t.Fatal(err)
	}
	fullTarget := model.Target{BaseSHA: baselineTarget.BaseSHA, HeadSHA: baselineTarget.HeadSHA, DiffHash: "complete", Finalizable: true}
	manifest := model.Manifest{
		AutoFixLoopID: "loop", ReviewScope: "approved-baseline-delta",
		ApprovalBaselineRunID: run.ID, ApprovalBaselineHash: baselineTarget.DiffHash,
		Target:     model.Target{BaseSHA: baselineTarget.HeadSHA, HeadSHA: baselineTarget.HeadSHA, DiffHash: "delta", Finalizable: true},
		FullTarget: &fullTarget,
	}

	context, trustedTarget, err := retryAutoFixReviewContext(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if context.TrustedBaseSHA != baselineTarget.BaseSHA || context.FullTarget.DiffHash != fullTarget.DiffHash || len(context.BaselineFindings) != 1 || trustedTarget.BaseSHA != baselineTarget.BaseSHA {
		t.Fatalf("restored retry context = %#v, trusted target = %#v", context, trustedTarget)
	}
}

func TestRetryAutoFixReviewContextRejectsTruncatedScopedRecord(t *testing.T) {
	_, _, err := retryAutoFixReviewContext(record.New(t.TempDir()), model.Manifest{
		AutoFixLoopID: "loop", ReviewScope: "approved-baseline-delta",
		Target: model.Target{BaseSHA: "untrusted-head"},
	})
	if err == nil || !strings.Contains(err.Error(), "complete target") {
		t.Fatalf("truncated scoped retry error = %v", err)
	}
}

func TestDeltaApprovalIsNonFinalInCLIAndCannotVerify(t *testing.T) {
	store := record.New(t.TempDir())
	patch := []byte("diff --git a/app.go b/app.go\n")
	sum := sha256.Sum256(patch)
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: hex.EncodeToString(sum[:]), Finalizable: true}
	run, err := store.Create(time.Unix(10, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	report := &model.ReviewReport{Verdict: "approve", ContextComplete: true}
	if err := record.WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{
		RunID: run.ID, RepositoryIdentity: "github.com/example/project", Target: target, ReviewScope: "approved-baseline-delta", AutoFixLoopID: "loop-1", AutoFixIteration: 2,
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Report: report},
			{Reviewer: "claude", Status: "completed", Report: report},
		},
	}); err != nil {
		t.Fatal(err)
	}
	decision := model.Decision{
		RunID: run.ID, State: model.StateApproved, Reason: "delta reviewers agree",
		BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"},
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := findApproval(store, run.ID, target.HeadSHA, "github.com/example/project"); err == nil {
		t.Fatal("delta-only approval was accepted for verification")
	}
	summary, err := loadRunSummary(run)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != stateDeltaApproved || summary.Phase != "final-full-review-required" {
		t.Fatalf("delta summary = %#v", summary)
	}
	display := decisionForDisplay(decision, "approved-baseline-delta")
	if display.State != stateDeltaApproved || display.OutcomeQualifier != "final_full_review_required" || !strings.Contains(display.Reason, "non-final") {
		t.Fatalf("delta display decision = %#v", display)
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

func TestPreserveRetryReviewerSettingsKeepsSecurityEscalation(t *testing.T) {
	cfg := config.Defaults()
	cfg.Escalation.Enabled = false
	cfg.Escalation.Model = "new-default"
	cfg.Escalation.Effort = "medium"
	manifest := model.Manifest{
		Escalation: model.EscalationMetadata{Triggered: true, Causes: []string{"security_sensitive"}},
	}
	lineage := record.ReviewerLineage{
		SecurityReviews:       []model.ReviewerResult{{Reviewer: "claude-security", Model: "claude-fable-5", Effort: "high", EscalationCause: "security_sensitive"}},
		LatestSecurityReviews: []model.ReviewerResult{{Reviewer: "claude-security", Status: "incomplete", EscalationCause: "security_sensitive"}},
	}

	preserveRetryReviewerSettings(&cfg, manifest, lineage, map[string]bool{"claude-security": true})

	if !cfg.Escalation.Enabled || !cfg.Escalation.ForceSecuritySensitive || cfg.Escalation.Model != "claude-fable-5" || cfg.Escalation.Effort != "high" {
		t.Fatalf("preserved escalation = %#v", cfg.Escalation)
	}
}

func TestPreserveRetryReviewerSettingsDoesNotEscalateRoutineReview(t *testing.T) {
	cfg := config.Defaults()
	want := cfg.Escalation
	manifest := model.Manifest{Reviewers: []model.ReviewerResult{{
		Reviewer: "claude", Model: "claude-opus-4-6", Effort: "high",
	}}}
	lineage := record.ReviewerLineage{Reviewers: manifest.Reviewers, LatestReviewers: manifest.Reviewers}

	preserveRetryReviewerSettings(&cfg, manifest, lineage, map[string]bool{"claude": true})

	if cfg.Escalation.Enabled != want.Enabled || cfg.Escalation.ForceSecuritySensitive != want.ForceSecuritySensitive || cfg.Escalation.Model != want.Model || cfg.Escalation.Effort != want.Effort {
		t.Fatalf("routine escalation changed from %#v to %#v", want, cfg.Escalation)
	}
}

func TestApplyRetryLimitOverridesRaisesSelectedRoleLimitsAndRecordsIntent(t *testing.T) {
	cfg := config.Defaults()
	escalationTurns := 35
	cfg.Escalation.MaxTurns = &escalationTurns
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	selected := map[string]bool{
		"claude": true, "claude-security": true, "claude-cross-examination": true,
	}

	overrides, err := applyRetryLimitOverrides(&cfg, limits, selected, retryLimitOverrideInput{
		ReviewerTimeout: 30 * time.Minute, ReviewerTimeoutSet: true,
		OverallTimeout: 90 * time.Minute, OverallTimeoutSet: true,
		MaxTurns: 60, MaxTurnsSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewerTimeout.Duration != 15*time.Minute || cfg.CrossExamination.Timeout.Duration != 10*time.Minute || cfg.OverallTimeout.Duration != 90*time.Minute {
		t.Fatalf("role-specific retry mutated shared policy = reviewer %s cross %s overall %s", cfg.ReviewerTimeout.Duration, cfg.CrossExamination.Timeout.Duration, cfg.OverallTimeout.Duration)
	}
	for _, reviewer := range []string{"claude", "claude-security", "claude-cross-examination"} {
		if limits[reviewer].Timeout.Duration != 30*time.Minute || limits[reviewer].MaxTurns != 60 {
			t.Fatalf("selected %s limits = %#v", reviewer, limits[reviewer])
		}
	}
	if limits["claude-escalation"].Timeout.Duration != 15*time.Minute || limits["claude-escalation"].MaxTurns != 35 {
		t.Fatalf("selected retry leaked to dispute adjudication: %#v", limits["claude-escalation"])
	}
	if overrides == nil || overrides.ReviewerTimeout == nil || overrides.OverallTimeout == nil || overrides.MaxTurns == nil || !slices.Equal(overrides.Reviewers, []string{"claude", "claude-cross-examination", "claude-security"}) {
		t.Fatalf("recorded retry overrides = %#v", overrides)
	}
}

func TestApplyRetryLimitOverridesRejectsWeakeningAndIneffectiveTimeouts(t *testing.T) {
	tests := []struct {
		name     string
		selected map[string]bool
		input    retryLimitOverrideInput
		want     string
	}{
		{name: "timeout must rise", selected: map[string]bool{"claude": true}, input: retryLimitOverrideInput{ReviewerTimeout: 10 * time.Minute, ReviewerTimeoutSet: true}, want: "must raise"},
		{name: "overall must rise", selected: map[string]bool{"claude": true}, input: retryLimitOverrideInput{OverallTimeout: 30 * time.Minute, OverallTimeoutSet: true}, want: "must raise"},
		{name: "reviewer timeout must fit overall", selected: map[string]bool{"claude": true}, input: retryLimitOverrideInput{ReviewerTimeout: time.Hour, ReviewerTimeoutSet: true}, want: "also raise --overall-timeout"},
		{name: "turns require Claude", selected: map[string]bool{"codex": true}, input: retryLimitOverrideInput{MaxTurns: 60, MaxTurnsSet: true}, want: "Claude-backed"},
		{name: "turns must rise", selected: map[string]bool{"claude": true}, input: retryLimitOverrideInput{MaxTurns: 40, MaxTurnsSet: true}, want: "must raise"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			limits := config.SnapshotReviewerExecutionLimits(cfg)
			_, err := applyRetryLimitOverrides(&cfg, limits, test.selected, test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("override error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestApplyRetryTimeoutOverrideDoesNotChangeUnselectedRoleLimit(t *testing.T) {
	cfg := config.Defaults()
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	_, err := applyRetryLimitOverrides(&cfg, limits, map[string]bool{"claude-cross-examination": true}, retryLimitOverrideInput{
		ReviewerTimeout: 12 * time.Minute, ReviewerTimeoutSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits["claude-cross-examination"].Timeout.Duration != 12*time.Minute || limits["claude"].Timeout.Duration != 15*time.Minute || cfg.CrossExamination.Timeout.Duration != 10*time.Minute {
		t.Fatalf("targeted timeout leaked to unselected role: limits=%#v config=%s", limits, cfg.CrossExamination.Timeout.Duration)
	}
}

func TestRetryExecutionLimitsRemainRoleSpecificAcrossGenerations(t *testing.T) {
	cfg := config.Defaults()
	escalationTurns := 35
	cfg.Escalation.MaxTurns = &escalationTurns
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	_, err := applyRetryLimitOverrides(&cfg, limits, map[string]bool{"claude-security": true}, retryLimitOverrideInput{
		ReviewerTimeout: 30 * time.Minute, ReviewerTimeoutSet: true,
		OverallTimeout: 90 * time.Minute, OverallTimeoutSet: true,
		MaxTurns: 60, MaxTurnsSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.SnapshotReviewPolicy(cfg)
	child := model.Manifest{ReviewPolicy: &policy, ReviewerExecutionLimits: limits}

	nextCfg, err := config.ApplyReviewPolicy(config.Defaults(), *child.ReviewPolicy)
	if err != nil {
		t.Fatal(err)
	}
	nextLimits := savedReviewerExecutionLimits(nextCfg, child)
	if nextLimits["claude-security"].Timeout.Duration != 30*time.Minute || nextLimits["claude-security"].MaxTurns != 60 {
		t.Fatalf("security limits were not inherited: %#v", nextLimits["claude-security"])
	}
	for _, reviewer := range []string{"claude", "claude-escalation"} {
		if nextLimits[reviewer].Timeout.Duration != 15*time.Minute || nextLimits[reviewer].MaxTurns != map[string]int{"claude": 50, "claude-escalation": 35}[reviewer] {
			t.Fatalf("security retry leaked into %s: %#v", reviewer, nextLimits[reviewer])
		}
	}
	if nextLimits["claude-cross-examination"].Timeout.Duration != 10*time.Minute || nextLimits["claude-cross-examination"].MaxTurns != 20 {
		t.Fatalf("security retry leaked into cross-examination: %#v", nextLimits["claude-cross-examination"])
	}

	_, err = applyRetryLimitOverrides(&nextCfg, nextLimits, map[string]bool{"claude-escalation": true}, retryLimitOverrideInput{
		ReviewerTimeout: 35 * time.Minute, ReviewerTimeoutSet: true,
		MaxTurns: 65, MaxTurnsSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	grandchildPolicy := config.SnapshotReviewPolicy(nextCfg)
	grandchild := model.Manifest{ReviewPolicy: &grandchildPolicy, ReviewerExecutionLimits: nextLimits}
	thirdCfg, err := config.ApplyReviewPolicy(config.Defaults(), *grandchild.ReviewPolicy)
	if err != nil {
		t.Fatal(err)
	}
	grandchildLimits := savedReviewerExecutionLimits(thirdCfg, grandchild)
	if grandchildLimits["claude-security"].Timeout.Duration != 30*time.Minute || grandchildLimits["claude-security"].MaxTurns != 60 {
		t.Fatalf("second-generation adjudication override changed security limits: %#v", grandchildLimits["claude-security"])
	}
	if grandchildLimits["claude-escalation"].Timeout.Duration != 35*time.Minute || grandchildLimits["claude-escalation"].MaxTurns != 65 {
		t.Fatalf("second-generation adjudication limits = %#v", grandchildLimits["claude-escalation"])
	}
}

func TestShowInActiveStatusIncludesQuotaQueuedAndPausedRuns(t *testing.T) {
	tests := []struct {
		name    string
		summary model.RunSummary
		want    bool
	}{
		{name: "active", summary: model.RunSummary{State: "active"}, want: true},
		{name: "quota queued state", summary: model.RunSummary{State: "quota-queued"}, want: true},
		{name: "paused state", summary: model.RunSummary{State: "paused"}, want: true},
		{name: "quota queued reviewer survives stale heartbeat classification", summary: model.RunSummary{State: "interrupted", Reviewers: map[string]string{"claude": "quota-queued"}}, want: true},
		{name: "finished failure with stale reviewer state", summary: model.RunSummary{State: "failed", Reviewers: map[string]string{"claude": "quota-queued"}}, want: false},
		{name: "complete", summary: model.RunSummary{State: model.StateApproved}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := showInActiveStatus(test.summary); got != test.want {
				t.Fatalf("showInActiveStatus(%#v) = %v, want %v", test.summary, got, test.want)
			}
		})
	}
}

func TestLoadAutoFixSummariesKeepsPausedQuotaLoopVisible(t *testing.T) {
	store := record.New(t.TempDir())
	now := time.Now().UTC()
	run, err := store.CreateAutoFixLoop(now.Add(-time.Hour), "abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	pausedAt := now.Add(-30 * time.Minute)
	retryAt := now.Add(time.Hour)
	loop := model.AutoFixLoop{
		LoopID: run.ID, State: model.StatePaused, StartedAt: now.Add(-time.Hour),
		InitialHeadSHA: "abcdef123456", PausedAt: &pausedAt, RetryAt: &retryAt,
		PausedDuration: model.NewDuration(5 * time.Minute), ResumePhase: "review",
		ResumeReviewers: []string{"claude-security"}, RecordPath: run.Path,
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), loop); err != nil {
		t.Fatal(err)
	}
	summaries, err := loadAutoFixSummaries(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("auto-fix summaries = %#v", summaries)
	}
	summary := summaries[0]
	if summary.State != model.StatePaused || summary.Phase != "paused-review" || summary.Reviewers["claude-security"] != "quota-queued" || summary.Queues["claude-security"].ETAAt == nil {
		t.Fatalf("paused summary = %#v", summary)
	}
	if summary.ElapsedMS < (59*time.Minute).Milliseconds() || summary.ActiveExecutionMS < (24*time.Minute).Milliseconds() || summary.ActiveExecutionMS > (26*time.Minute).Milliseconds() {
		t.Fatalf("paused timing summary = %#v", summary)
	}
}

func TestLoadAutoFixSummariesMarksElapsedQuotaPauseRetryReady(t *testing.T) {
	store := record.New(t.TempDir())
	now := time.Now().UTC()
	run, err := store.CreateAutoFixLoop(now.Add(-time.Hour), "abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	pausedAt := now.Add(-30 * time.Minute)
	retryAt := now.Add(-time.Minute)
	loop := model.AutoFixLoop{
		LoopID: run.ID, State: model.StatePaused, StartedAt: now.Add(-time.Hour), InitialHeadSHA: "abcdef123456",
		PausedAt: &pausedAt, RetryAt: &retryAt, ResumePhase: "review", ResumeReviewers: []string{"claude-security"}, RecordPath: run.Path,
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), loop); err != nil {
		t.Fatal(err)
	}
	summaries, err := loadAutoFixSummaries(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("auto-fix summaries = %#v", summaries)
	}
	summary := summaries[0]
	if !summary.AwaitingResume || !summary.RetryReady || summary.Phase != "awaiting-resume" || summary.Reviewers["claude-security"] != "retry-ready" || len(summary.Queues) != 0 || summary.RetryAt == nil {
		t.Fatalf("elapsed quota pause still looks capacity-queued: %#v", summary)
	}
}

func TestManifestReviewerResultsIncludesTargetedSecurityReview(t *testing.T) {
	results := manifestReviewerResults(model.Manifest{
		Reviewers:         []model.ReviewerResult{{Reviewer: "codex"}, {Reviewer: "claude"}},
		SecurityReviews:   []model.ReviewerResult{{Reviewer: "claude-security"}},
		CrossExaminations: []model.ReviewerResult{{Reviewer: "claude-cross-examination"}},
	})
	if len(results) != 4 || results[2].Reviewer != "claude-security" || results[3].Reviewer != "claude-cross-examination" {
		t.Fatalf("manifest reviewer results = %#v", results)
	}
}

func TestExitCodeForState(t *testing.T) {
	tests := map[string]int{
		model.StateApproved:         0,
		model.StateChangesRequested: 2,
		model.StateNeedsHuman:       3,
		model.StateIncomplete:       4,
		model.StateStale:            5,
		model.StatePaused:           6,
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
allow_review_web = true
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
	if cfg.Reviewers.Claude.Enabled || cfg.MinimumApprovals != 1 || cfg.AllowReviewWeb || len(cfg.Checks) != 0 {
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
