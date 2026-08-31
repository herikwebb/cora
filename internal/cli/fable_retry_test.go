package cli

import (
	"testing"

	"github.com/herikwebb/cora/internal/model"
)

func TestSelectRetryReviewersIncludesSpecializedFablePhases(t *testing.T) {
	results := []model.ReviewerResult{
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude-escalation", Status: "deferred", FailureKind: "dependency_changed", Retryable: true},
		{Reviewer: "claude-cross-examination", Status: "incomplete", FailureKind: "quota", Retryable: true},
	}

	selected, err := selectRetryReviewers(results, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || !selected["claude-escalation"] || !selected["claude-cross-examination"] {
		t.Fatalf("default specialized retry selection = %#v", selected)
	}

	for _, reviewer := range []string{"claude-escalation", "claude-cross-examination"} {
		t.Run(reviewer, func(t *testing.T) {
			explicit, err := selectRetryReviewers(results, []string{reviewer})
			if err != nil {
				t.Fatal(err)
			}
			if len(explicit) != 1 || !explicit[reviewer] {
				t.Fatalf("explicit specialized retry selection = %#v", explicit)
			}
		})
	}
}
