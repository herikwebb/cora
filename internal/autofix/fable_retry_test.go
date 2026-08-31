package autofix

import (
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestQuotaResumeReviewersProactivelySelectsDependentFablePhases(t *testing.T) {
	cfg := config.Defaults()
	cfg.Escalation.Enabled = true
	cfg.Escalation.AdjudicateDisagreements = true
	cfg.CrossExamineBlockingFindings = true
	policy := config.SnapshotReviewPolicy(cfg)
	retryAt := time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC)
	manifest := model.Manifest{
		ReviewPolicy: &policy,
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
			{Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Retryable: true, RetryAt: &retryAt},
		},
		Checks: []model.CheckResult{{Name: "test", Status: "passed"}},
	}

	gotRetryAt, reviewers, ok := quotaResumeReviewers(manifest)
	if !ok || gotRetryAt == nil || !gotRetryAt.Equal(retryAt) {
		t.Fatalf("quota resume = retry_at %v reviewers %v ok %t", gotRetryAt, reviewers, ok)
	}
	if got := strings.Join(reviewers, ","); got != "claude,claude-cross-examination,claude-escalation" {
		t.Fatalf("proactive downstream retry selection = %q", got)
	}
	if !containsRetryableReviewer(manifest, map[string]bool{
		"claude": true, "claude-escalation": true, "claude-cross-examination": true,
	}) {
		t.Fatal("quota resume should require every potentially downstream specialized phase")
	}
	if containsRetryableReviewer(manifest, map[string]bool{"claude": true}) {
		t.Fatal("quota resume must not omit downstream adjudication and cross-examination")
	}
}
