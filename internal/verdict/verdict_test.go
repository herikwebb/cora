package verdict

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestEvaluate(t *testing.T) {
	approve := completedReview("codex", "approve")
	claudeApprove := completedReview("claude", "approve")
	requestChanges := completedReview("claude", "request_changes")
	abstain := completedReview("claude", "abstain")
	incomplete := model.ReviewerResult{Reviewer: "claude", Status: "incomplete"}
	majorFinding := completedReview("claude", "approve")
	majorFinding.Report.Findings = []model.Finding{{ID: "F1", Severity: "major", Claim: "bug", Evidence: "line 1"}}

	tests := []struct {
		name      string
		target    model.Target
		reviewers []model.ReviewerResult
		checks    []model.CheckResult
		minimum   int
		want      string
	}{
		{name: "consensus", target: finalTarget(), reviewers: []model.ReviewerResult{approve, claudeApprove}, minimum: 2, want: model.StateApproved},
		{name: "explicit request outranks approval", target: finalTarget(), reviewers: []model.ReviewerResult{approve, requestChanges}, minimum: 2, want: model.StateChangesRequested},
		{name: "blocking finding", target: finalTarget(), reviewers: []model.ReviewerResult{approve, majorFinding}, minimum: 2, want: model.StateChangesRequested},
		{name: "request outranks abstention", target: finalTarget(), reviewers: []model.ReviewerResult{requestChanges, abstain}, minimum: 2, want: model.StateChangesRequested},
		{name: "abstention needs human", target: finalTarget(), reviewers: []model.ReviewerResult{approve, abstain}, minimum: 2, want: model.StateNeedsHuman},
		{name: "incomplete fails closed", target: finalTarget(), reviewers: []model.ReviewerResult{requestChanges, incomplete}, minimum: 2, want: model.StateIncomplete},
		{name: "failed check", target: finalTarget(), reviewers: []model.ReviewerResult{approve, claudeApprove}, checks: []model.CheckResult{{Name: "test", Status: "failed"}}, minimum: 2, want: model.StateChangesRequested},
		{name: "quorum not met", target: finalTarget(), reviewers: []model.ReviewerResult{approve}, minimum: 2, want: model.StateIncomplete},
		{name: "working tree is advisory", target: model.Target{Finalizable: false}, reviewers: []model.ReviewerResult{approve, claudeApprove}, minimum: 2, want: model.StateIncomplete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Evaluate("run", test.target, test.reviewers, test.checks, []string{"blocker", "major"}, test.minimum, time.Unix(1, 0))
			if decision.State != test.want {
				t.Fatalf("state = %q, want %q (reason: %s)", decision.State, test.want, decision.Reason)
			}
		})
	}
}

func TestEvaluateDeduplicatesEquivalentFindings(t *testing.T) {
	codex := completedReview("codex", "request_changes")
	codex.Report.Findings = []model.Finding{{
		ID: "C1", Severity: "major", Confidence: 0.9, File: "app.go", Line: 42,
		Claim: "Concurrent writes can corrupt the shared cache", Evidence: "Both goroutines write the map", SuggestedFix: "Add a mutex",
	}}
	claude := completedReview("claude", "request_changes")
	claude.Report.Findings = []model.Finding{{
		ID: "A1", Severity: "minor", Confidence: 0.8, File: "app.go", Line: 43,
		Claim: "Shared cache can be corrupted by concurrent writes", Evidence: "The map is written without synchronization", SuggestedFix: "Protect it with a lock",
	}}
	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{codex, claude}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if len(decision.Findings) != 1 || decision.OpenFindings["major"] != 1 || decision.OpenFindings["minor"] != 0 {
		t.Fatalf("consolidated findings = %#v, counts = %#v", decision.Findings, decision.OpenFindings)
	}
	finding := decision.Findings[0]
	if len(finding.Reviewers) != 2 || len(finding.Evidence) != 2 || len(finding.SuggestedFixes) != 2 {
		t.Fatalf("merged finding lost source detail: %#v", finding)
	}
}

func TestEvaluateDeduplicatesSemanticReviewFindings(t *testing.T) {
	tests := []struct {
		name, file, left, right string
		line                    int
	}{
		{
			name: "scheduled workflow namespace binding", file: "backend/src/apiserver/server/run_server.go", line: 757,
			left:  "The new destination-namespace rule does not close the scheduled-workflow confused-deputy path: destinationNamespace is derived from the attacker-controlled experiment ID rather than the ScheduledWorkflow namespace.",
			right: "The confused-deputy protection compares the referenced pipeline only with the requested destination namespace, without binding either value to the namespace of the ScheduledWorkflow.",
		},
		{
			name: "vacuous deletion assertion", file: "backend/src/apiserver/resource/resource_manager_test.go", line: 4373,
			left:  "The workflow still exists assertion is vacuous because the fake Delete never removes the entry.",
			right: "The test does not verify that the workflow was left undeleted; its post-condition passes even if a delete was issued.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := completedReview("codex", "approve")
			first.Report.Findings = []model.Finding{{ID: "C", Severity: "minor", File: test.file, Line: test.line, Claim: test.left}}
			second := completedReview("claude", "approve")
			second.Report.Findings = []model.Finding{{ID: "A", Severity: "minor", File: test.file, Line: test.line + 1, Claim: test.right}}
			decision := Evaluate("run", finalTarget(), []model.ReviewerResult{first, second}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
			if len(decision.Findings) != 1 {
				t.Fatalf("findings were not merged: %#v", decision.Findings)
			}
		})
	}
}

func TestEvaluateKeepsDifferentDefectsAtSameLine(t *testing.T) {
	first := completedReview("codex", "approve")
	first.Report.Findings = []model.Finding{{ID: "C", Severity: "minor", File: "server.go", Line: 100, Claim: "The cache write races with concurrent readers and can panic."}}
	second := completedReview("claude", "approve")
	second.Report.Findings = []model.Finding{{ID: "A", Severity: "minor", File: "server.go", Line: 100, Claim: "The returned authorization error exposes the tenant namespace."}}
	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{first, second}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if len(decision.Findings) != 2 {
		t.Fatalf("different defects were merged: %#v", decision.Findings)
	}
}

func TestEvaluateQualifiesApprovalWithNonBlockingFindings(t *testing.T) {
	codex := completedReview("codex", "approve")
	codex.Report.Findings = []model.Finding{{ID: "C", Severity: "minor", File: "app.go", Line: 5, Claim: "The diagnostic omits useful context."}}
	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{codex, completedReview("claude", "approve")}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if decision.State != model.StateApproved || decision.OutcomeQualifier != "non_blocking_findings" {
		t.Fatalf("qualified approval = %#v", decision)
	}
}

func TestTargetedSecurityReviewIsRequiredButDoesNotInflateOrdinaryQuorum(t *testing.T) {
	security := completedReview("claude-security", "approve")
	security.EscalationCause = "security_sensitive"

	withoutOrdinaryQuorum := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), security},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if withoutOrdinaryQuorum.State != model.StateIncomplete || withoutOrdinaryQuorum.Reason != "approval quorum not met: got 1, need 2" {
		t.Fatalf("targeted approval satisfied ordinary quorum: %#v", withoutOrdinaryQuorum)
	}

	withOrdinaryQuorum := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve"), security},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if withOrdinaryQuorum.State != model.StateApproved {
		t.Fatalf("required targeted review prevented valid ordinary quorum: %#v", withOrdinaryQuorum)
	}

	security.Status = "incomplete"
	security.Report = nil
	security.Error = "security review timed out"
	failClosed := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve"), security},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if failClosed.State != model.StateIncomplete {
		t.Fatalf("incomplete targeted security review did not fail closed: %#v", failClosed)
	}
}

func TestDeferredTargetedReviewPreservesKnownChangesButBlocksLaterApproval(t *testing.T) {
	deferred := model.ReviewerResult{
		Reviewer: "claude-security", Status: "deferred", FailureKind: "outcome_fixed",
		Error: "ordinary findings already require changes", EscalationCause: "security_sensitive",
	}
	changes := completedReview("codex", "request_changes")
	changes.Report.Findings = []model.Finding{{ID: "major", Severity: "major", File: "app.go", Line: 1, Claim: "reachable defect"}}
	withKnownChanges := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{changes, completedReview("claude", "approve"), deferred},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if withKnownChanges.State != model.StateChangesRequested || withKnownChanges.Reviewers["claude-security"] != "deferred" {
		t.Fatalf("deferred security review obscured known changes: %#v", withKnownChanges)
	}

	withoutChanges := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve"), deferred},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if withoutChanges.State != model.StateIncomplete || !strings.Contains(withoutChanges.Reason, "deferred") {
		t.Fatalf("deferred required review allowed approval: %#v", withoutChanges)
	}
}

func TestEvaluateTreatsMinorAsBlockingWhenPolicyIncludesIt(t *testing.T) {
	codex := completedReview("codex", "approve")
	codex.Report.Findings = []model.Finding{{ID: "C", Severity: "minor", File: "app.go", Line: 5, Claim: "Missing validation"}}
	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{codex, completedReview("claude", "approve")}, nil, []string{"blocker", "major", "minor"}, 2, time.Unix(1, 0))
	if decision.State != model.StateChangesRequested {
		t.Fatalf("strict minor decision = %#v", decision)
	}
}

func TestCrossExaminationCanDisproveUncorroboratedMajor(t *testing.T) {
	reporter := completedReview("codex", "request_changes")
	reporter.Report.Findings = []model.Finding{blockingFinding("C1")}
	reviewers := []model.ReviewerResult{reporter, completedReview("claude", "approve")}
	candidate := BlockingCandidates(reviewers)[0]
	examinations := []model.CrossExamination{{
		FindingID: candidate.ID, Reviewer: "claude-cross-examination", Status: "completed",
		Disposition: "disproved", OriginalSeverity: "major", EffectiveSeverity: "note",
		Rationale: "the validated value is used instead", Reachability: &model.Reachability{
			Status: "not_demonstrated", Path: []string{"handler.go:20 rejects the value"}, Impact: "the alleged sink is never reached",
		},
	}}
	decision := EvaluateWithCrossExaminations("run", finalTarget(), reviewers, nil, []string{"blocker", "major"}, 2, examinations, time.Unix(1, 0))
	if decision.State != model.StateApproved || decision.OutcomeQualifier != "cross_examined" {
		t.Fatalf("cross-examined decision = %#v", decision)
	}
	if len(decision.Findings) != 0 || len(decision.RejectedFindings) != 1 || decision.RejectedFindings[0].Disposition != "disproved" {
		t.Fatalf("rejected findings = %#v, open = %#v", decision.RejectedFindings, decision.Findings)
	}
}

func TestCrossExaminationDemotionRespectsBlockingPolicy(t *testing.T) {
	reporter := completedReview("codex", "request_changes")
	reporter.Report.Findings = []model.Finding{blockingFinding("C1")}
	reviewers := []model.ReviewerResult{reporter, completedReview("claude", "approve")}
	candidate := BlockingCandidates(reviewers)[0]
	examinations := []model.CrossExamination{{
		FindingID: candidate.ID, Reviewer: "claude-cross-examination", Status: "completed",
		Disposition: "demoted", OriginalSeverity: "major", EffectiveSeverity: "minor",
		Rationale: "the behavior is reachable but has only diagnostic impact",
	}}

	standard := EvaluateWithCrossExaminations("run", finalTarget(), reviewers, nil, []string{"blocker", "major"}, 2, examinations, time.Unix(1, 0))
	if standard.State != model.StateApproved || len(standard.Findings) != 1 || standard.Findings[0].Severity != "minor" {
		t.Fatalf("standard demotion decision = %#v", standard)
	}
	strict := EvaluateWithCrossExaminations("run", finalTarget(), reviewers, nil, []string{"blocker", "major", "minor"}, 2, examinations, time.Unix(1, 0))
	if strict.State != model.StateChangesRequested {
		t.Fatalf("strict demotion decision = %#v", strict)
	}
}

func TestCrossExaminationConfirmedOrIncompleteFailsClosed(t *testing.T) {
	reporter := completedReview("codex", "request_changes")
	reporter.Report.Findings = []model.Finding{blockingFinding("C1")}
	reviewers := []model.ReviewerResult{reporter, completedReview("claude", "approve")}
	candidate := BlockingCandidates(reviewers)[0]

	confirmed := []model.CrossExamination{{
		FindingID: candidate.ID, Reviewer: "claude-cross-examination", Status: "completed",
		Disposition: "confirmed", OriginalSeverity: "major", EffectiveSeverity: "major",
		Reachability: blockingFinding("cross").Reachability,
	}}
	decision := EvaluateWithCrossExaminations("run", finalTarget(), reviewers, nil, []string{"blocker", "major"}, 2, confirmed, time.Unix(1, 0))
	if decision.State != model.StateChangesRequested {
		t.Fatalf("confirmed decision = %#v", decision)
	}

	incomplete := []model.CrossExamination{{
		FindingID: candidate.ID, Reviewer: "claude-cross-examination", Status: "incomplete",
		OriginalSeverity: "major", Error: "cross-examiner timed out",
	}}
	decision = EvaluateWithCrossExaminations("run", finalTarget(), reviewers, nil, []string{"blocker", "major"}, 2, incomplete, time.Unix(1, 0))
	if decision.State != model.StateIncomplete {
		t.Fatalf("incomplete cross-examination decision = %#v", decision)
	}
}

func TestBlockingCandidatesExcludesCorroboratedFindings(t *testing.T) {
	codex := completedReview("codex", "request_changes")
	codex.Report.Findings = []model.Finding{blockingFinding("C1")}
	claude := completedReview("claude", "request_changes")
	claudeFinding := blockingFinding("A1")
	claudeFinding.Line = codex.Report.Findings[0].Line + 1
	claude.Report.Findings = []model.Finding{claudeFinding}
	if candidates := BlockingCandidates([]model.ReviewerResult{codex, claude}); len(candidates) != 0 {
		t.Fatalf("corroborated finding was selected for cross-examination: %#v", candidates)
	}
}

func TestBlockingCandidatesExcludeTargetedSecurityFindings(t *testing.T) {
	security := completedReview("claude-security", "request_changes")
	security.EscalationCause = "security_sensitive"
	security.Report.Findings = []model.Finding{blockingFinding("SEC-1")}

	if candidates := BlockingCandidates([]model.ReviewerResult{security}); len(candidates) != 0 {
		t.Fatalf("targeted Fable finding triggered a redundant Fable cross-examination: %#v", candidates)
	}
	decision := Evaluate(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve"), security},
		nil, []string{"blocker", "major"}, 2, time.Unix(1, 0),
	)
	if decision.State != model.StateChangesRequested || decision.OpenFindings["major"] != 1 {
		t.Fatalf("targeted security finding did not block directly: %#v", decision)
	}
}

func TestBlockingCandidatesDeduplicatesParaphrasedFindingsAcrossSeverityAndLocations(t *testing.T) {
	codex := completedReview("codex", "request_changes")
	codex.Report.Findings = []model.Finding{{
		ID: "CORA-001", Severity: "major", Confidence: 0.99,
		File: "backend/src/apiserver/resource/resource_manager.go", Line: 2242,
		Claim:        "A V2 run remains permanently nonterminal if its Workflow disappears before the first successful workflow report.",
		Evidence:     "CreateRun stores a V2 Workflow only in PipelineRuntimeManifest, leaving WorkflowRuntimeManifest empty. A terminal report after deletion reaches validateStoredWorkflowReportIdentity, which rejects the empty manifest before UpdateRun can persist the terminal state.",
		SuggestedFix: "Validate against PipelineRuntimeManifest when WorkflowRuntimeManifest is empty.",
		Reachability: &model.Reachability{
			Status:  "demonstrated",
			Trigger: "A terminal snapshot of a V2 run arrives after its Workflow was deleted and before an earlier report succeeded.",
			Path: []string{
				"CreateRun writes PipelineRuntimeManifest but leaves WorkflowRuntimeManifest empty",
				"validateStoredWorkflowReportIdentity rejects the empty stored manifest",
			},
			Impact: "The run remains nonterminal and the persistence agent retries forever.",
		},
	}}
	claude := completedReview("claude", "approve")
	claude.Report.Findings = []model.Finding{{
		ID: "v2-stored-manifest-fallback", Severity: "minor", Confidence: 0.75,
		File: "backend/src/apiserver/resource/resource_manager.go", Line: 1810,
		Claim:        "The stored-identity fallback for finalizing a run whose workflow was deleted can never succeed for a V2 run that had no prior successful report, because WorkflowRuntimeManifest is empty; the run stays non-terminal while the persistence agent retries forever.",
		Evidence:     "Only V1 creation sets WorkflowRuntimeManifest. The terminal-report fallback calls validateStoredWorkflowReportIdentity, which returns an internal error for the empty manifest before the run update.",
		SuggestedFix: "When WorkflowRuntimeManifest is empty, fall back to the V2 PipelineRuntimeManifest.",
		Reachability: &model.Reachability{
			Status:  "demonstrated",
			Trigger: "A V2 workflow is deleted before the persistence agent completes a successful report.",
			Path: []string{
				"terminal report enters the stored identity fallback",
				"validateStoredWorkflowReportIdentity finds an empty WorkflowRuntimeManifest",
			},
			Impact: "The run cannot be finalized and its report is retried indefinitely.",
		},
	}}

	reviewers := []model.ReviewerResult{codex, claude}
	if candidates := BlockingCandidates(reviewers); len(candidates) != 0 {
		t.Fatalf("semantically corroborated finding was selected for cross-examination: %#v", candidates)
	}
	decision := Evaluate("run", finalTarget(), reviewers, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if len(decision.Findings) != 1 || len(decision.Findings[0].Reviewers) != 2 || decision.Findings[0].Severity != "major" {
		t.Fatalf("paraphrased findings were not consolidated: %#v", decision.Findings)
	}
}

func TestBlockingCandidatesKeepsUnrelatedDistantFindingsSeparate(t *testing.T) {
	codex := completedReview("codex", "request_changes")
	codex.Report.Findings = []model.Finding{{
		ID: "race", Severity: "major", File: "server.go", Line: 100,
		Claim:    "Concurrent cache writes race with readers and can panic the server.",
		Evidence: "The request handler mutates a shared map without synchronization.",
	}}
	claude := completedReview("claude", "request_changes")
	claude.Report.Findings = []model.Finding{{
		ID: "auth", Severity: "major", File: "server.go", Line: 500,
		Claim:    "The download handler skips tenant authorization and exposes private data.",
		Evidence: "The handler reads an object using an unscoped user-provided identifier.",
	}}

	candidates := BlockingCandidates([]model.ReviewerResult{codex, claude})
	if len(candidates) != 2 {
		t.Fatalf("unrelated distant findings were merged: %#v", candidates)
	}
}

func TestConsolidationPreservesMajorReachabilityWhenMinorHasHigherConfidence(t *testing.T) {
	major := completedReview("codex", "request_changes")
	major.Report.Findings = []model.Finding{{
		ID: "major", Severity: "major", Confidence: 0.8, File: "server.go", Line: 100,
		Claim: "Untrusted input reaches the command runner.", Evidence: "The handler forwards request.Command without validation.",
		Reachability: &model.Reachability{
			Status: "demonstrated", Trigger: "A caller controls request.Command",
			Path: []string{"handler reads request.Command", "runner executes the value"}, Impact: "arbitrary command execution",
		},
	}}
	minor := completedReview("claude", "approve")
	minor.Report.Findings = []model.Finding{{
		ID: "minor", Severity: "minor", Confidence: 0.95, File: "server.go", Line: 101,
		Claim: "The request command reaches the runner without validation.", Evidence: "The handler forwards request.Command directly.",
	}}

	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{minor, major}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if len(decision.Findings) != 1 {
		t.Fatalf("consolidated findings = %#v", decision.Findings)
	}
	finding := decision.Findings[0]
	if finding.Severity != "major" || finding.Reachability == nil || finding.Reachability.Status != "demonstrated" || finding.Claim != major.Report.Findings[0].Claim {
		t.Fatalf("major representation was lost to higher-confidence minor: %#v", finding)
	}
}

func TestBlockingCandidatesWithCarriedAllowsExplicitHistoricalResolution(t *testing.T) {
	carried := model.ConsolidatedFinding{
		ID: "prior-race", Severity: "major", File: "scheduler.go", Line: 80,
		Claim:     "Recurring runs can overlap before the previous state update.",
		Reviewers: []string{"codex"}, CarriedFromRunIDs: []string{"run-prior"},
	}
	approvals := []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve")}
	candidates := BlockingCandidatesWithCarried(approvals, []model.ConsolidatedFinding{carried})
	if len(candidates) != 1 || candidates[0].ID != carried.ID {
		t.Fatalf("sole-reviewer carried candidate = %#v", candidates)
	}
	carried.Reviewers = []string{"claude", "codex"}
	if candidates := BlockingCandidatesWithCarried(approvals, []model.ConsolidatedFinding{carried}); len(candidates) != 0 {
		t.Fatalf("historically corroborated finding was selected again: %#v", candidates)
	}
}

func TestEvaluateRetainsPartialReportEvidenceAndFailsClosed(t *testing.T) {
	partial := model.ReviewerResult{
		Reviewer: "claude", Status: "partial", Error: "max turns",
		Report: &model.ReviewReport{
			Verdict: "abstain", ContextComplete: false,
			Findings:      []model.Finding{{ID: "A", Severity: "minor", File: "app.go", Line: 5, Claim: "Possible resource leak"}},
			ResidualRisks: []string{"auth.go was not reviewed"},
		},
	}
	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), partial}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if decision.State != model.StateIncomplete || decision.Reviewers["claude"] != "partial" || len(decision.Findings) != 1 || len(decision.ResidualRisks) != 1 {
		t.Fatalf("partial report was not retained fail-closed: %#v", decision)
	}
	if decision.ReviewerErrors["claude"] != "max turns" {
		t.Fatalf("partial reviewer error = %#v", decision.ReviewerErrors)
	}
}

func TestEvaluateCarriesUnresolvedFindingAcrossSameDiff(t *testing.T) {
	carried := model.ConsolidatedFinding{
		ID: "prior-race", Severity: "major", Confidence: 0.95, File: "scheduler.go", Line: 80,
		Claim:     "Recurring runs can overlap before the prior execution becomes visible.",
		Evidence:  []string{"The next tick reads stale state before the prior write commits."},
		Reviewers: []string{"codex"}, SourceIDs: []string{"C1"}, CarriedFromRunIDs: []string{"run-prior"},
	}
	decision := EvaluateWithCarriedFindings(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), completedReview("claude", "approve")},
		nil, []string{"blocker", "major"}, 2, nil, []model.ConsolidatedFinding{carried}, time.Unix(1, 0),
	)
	if decision.State != model.StateChangesRequested || len(decision.Findings) != 1 || decision.Findings[0].ID != carried.ID {
		t.Fatalf("carried finding decision = %#v", decision)
	}
}

func TestEvaluateMergesCurrentAndCarriedEquivalentFinding(t *testing.T) {
	current := completedReview("claude", "request_changes")
	current.Report.Findings = []model.Finding{{
		ID: "current", Severity: "major", Confidence: 0.8, File: "scheduler.go", Line: 81,
		Claim:    "The next recurring execution races the previous run's state update.",
		Evidence: "The scheduler reads the stale running marker.",
	}}
	carried := model.ConsolidatedFinding{
		ID: "prior", Severity: "major", Confidence: 0.95, File: "scheduler.go", Line: 80,
		Claim:     "Recurring runs can overlap before the previous state update.",
		Evidence:  []string{"The prior execution has not committed its running marker."},
		Reviewers: []string{"codex"}, CarriedFromRunIDs: []string{"run-prior"},
	}
	decision := EvaluateWithCarriedFindings(
		"run", finalTarget(), []model.ReviewerResult{completedReview("codex", "approve"), current},
		nil, []string{"blocker", "major"}, 2, nil, []model.ConsolidatedFinding{carried}, time.Unix(1, 0),
	)
	if len(decision.Findings) != 1 || len(decision.Findings[0].CarriedFromRunIDs) != 1 || len(decision.Findings[0].Reviewers) != 2 || len(decision.Findings[0].HistoricalFindingIDs) != 1 || decision.Findings[0].HistoricalFindingIDs[0] != carried.ID {
		t.Fatalf("current and carried findings were not merged: %#v", decision.Findings)
	}
}

func TestCarryForwardFindingsExcludePartialSeverityPromotion(t *testing.T) {
	completed := completedReview("codex", "approve")
	completed.Report.Findings = []model.Finding{{
		ID: "minor", Severity: "minor", Confidence: 0.8, File: "app.go", Line: 12,
		Claim: "The retry path can duplicate a harmless log entry.", Evidence: "Both retry branches emit the same log line.", SuggestedFix: "Deduplicate the log call.",
	}}
	partial := model.ReviewerResult{
		Reviewer: "claude", Status: "partial", Error: "review timed out",
		Report: &model.ReviewReport{
			SchemaVersion: model.SchemaVersion, Verdict: "abstain", ContextComplete: false,
			Findings: []model.Finding{{
				ID: "checkpoint-major", Severity: "major", Confidence: 0.99, File: "app.go", Line: 13,
				Claim: "The retry path can duplicate a harmless log entry.", Evidence: "Both retry branches emit the same log line.", SuggestedFix: "Deduplicate the log call.",
				Reachability: &model.Reachability{Status: "demonstrated", Trigger: "a retry occurs", Path: []string{"both retry branches log"}, Impact: "a duplicate log entry"},
			}},
		},
	}

	decision := Evaluate("run", finalTarget(), []model.ReviewerResult{completed, partial}, nil, []string{"blocker", "major"}, 2, time.Unix(1, 0))
	if decision.State != model.StateIncomplete || len(decision.Findings) != 1 || decision.Findings[0].Severity != "major" {
		t.Fatalf("current fail-closed decision = %#v", decision)
	}
	if len(decision.CarryForwardFindings) != 1 || decision.CarryForwardFindings[0].Severity != "minor" || !slices.Equal(decision.CarryForwardFindings[0].Reviewers, []string{"codex"}) {
		t.Fatalf("partial severity was promoted into durable history: %#v", decision.CarryForwardFindings)
	}
}

func TestIncompleteCrossExaminationPreservesOriginalReachability(t *testing.T) {
	review := completedReview("codex", "request_changes")
	originalReachability := &model.Reachability{
		Status: "demonstrated", Trigger: "an attacker submits input", Path: []string{"handler", "sink"}, Impact: "command execution",
	}
	review.Report.Findings = []model.Finding{{
		ID: "command", Severity: "major", Confidence: 0.95, File: "app.go", Line: 20,
		Claim: "Untrusted input reaches a command sink.", Evidence: "The handler forwards the value.", SuggestedFix: "Validate the value.", Reachability: originalReachability,
	}}
	candidate := BlockingCandidates([]model.ReviewerResult{review})[0]
	decision := EvaluateWithCrossExaminations(
		"run", finalTarget(), []model.ReviewerResult{review, completedReview("claude", "approve")}, nil,
		[]string{"blocker", "major"}, 2,
		[]model.CrossExamination{{FindingID: candidate.ID, Reviewer: "claude-cross-examination", Status: "incomplete", Error: "timeout"}},
		time.Unix(1, 0),
	)
	if decision.State != model.StateIncomplete || len(decision.Findings) != 1 || decision.Findings[0].Reachability == nil || decision.Findings[0].Reachability.Status != "demonstrated" {
		t.Fatalf("incomplete cross-examination erased original reachability: %#v", decision)
	}
}

func completedReview(name, reportVerdict string) model.ReviewerResult {
	return model.ReviewerResult{
		Reviewer: name,
		Status:   "completed",
		Report: &model.ReviewReport{
			SchemaVersion:   model.SchemaVersion,
			Verdict:         reportVerdict,
			ContextComplete: true,
		},
	}
}

func blockingFinding(id string) model.Finding {
	return model.Finding{
		ID: id, Severity: "major", Confidence: 0.95, File: "handler.go", Line: 20,
		Claim: "Untrusted command input reaches process execution", Evidence: "handler passes command into runner", SuggestedFix: "validate the command",
		Reachability: &model.Reachability{
			Status: "demonstrated", Trigger: "an authenticated request provides command input",
			Path:   []string{"handler.go:20 accepts input", "runner.go:45 invokes the process"},
			Impact: "attacker-selected input is executed", Preconditions: []string{"the runner is enabled"},
		},
	}
}

func finalTarget() model.Target {
	return model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff", Finalizable: true}
}
