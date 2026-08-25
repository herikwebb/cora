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
