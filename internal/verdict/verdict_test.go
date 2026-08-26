package verdict

import (
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

func finalTarget() model.Target {
	return model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff", Finalizable: true}
}
