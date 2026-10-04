package autofix

import (
	"reflect"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestGeminiQuotaResumeSelectsDependentReviews(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Escalation.AdjudicateDisagreements = true
	cfg.CrossExamineBlockingFindings = true
	policy := config.SnapshotReviewPolicy(cfg)
	retryAt := time.Now().Add(time.Hour)
	manifest := model.Manifest{ReviewPolicy: &policy, Reviewers: []model.ReviewerResult{
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "gemini", Status: "incomplete", FailureKind: "quota", Retryable: true, RetryAt: &retryAt},
	}}
	gotRetryAt, reviewers, ok := quotaResumeReviewers(manifest)
	want := []string{"claude-cross-examination", "claude-escalation", "gemini"}
	if !ok || gotRetryAt == nil || !gotRetryAt.Equal(retryAt) || !reflect.DeepEqual(reviewers, want) {
		t.Fatalf("Gemini quota resume = %v, %v, %t", gotRetryAt, reviewers, ok)
	}
	if containsRetryableReviewer(manifest, map[string]bool{"gemini": true}) {
		t.Fatal("Gemini quota resume omitted dependent review roles")
	}
}

func TestApprovedBaselineRequiresConfiguredGeminiApproval(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	policy := config.SnapshotReviewPolicy(cfg)
	results := []model.ReviewerResult{
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
	}
	if baselineReviewersMatchPolicy(results, policy) {
		t.Fatal("accepted baseline without required Gemini approval")
	}
	results = append(results, model.ReviewerResult{Reviewer: "gemini", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}})
	if !baselineReviewersMatchPolicy(results, policy) {
		t.Fatal("rejected baseline approved by every configured reviewer")
	}
	results[2].Report.Verdict = "request_changes"
	if baselineReviewersMatchPolicy(results, policy) {
		t.Fatal("accepted baseline despite Gemini requesting changes")
	}
}

func TestHistoricalPolicyWithoutGeminiMatchesDisabledGemini(t *testing.T) {
	current := config.SnapshotReviewPolicy(config.Defaults())
	historical := current
	historical.Gemini = model.AutoFixReviewerPolicy{}
	if !sameReviewPolicy(current, historical) {
		t.Fatal("disabled Gemini settings invalidated a historical policy")
	}
	current.Gemini.Enabled = true
	if sameReviewPolicy(current, historical) {
		t.Fatal("enabled Gemini was omitted from policy comparison")
	}
}

func TestGeminiControlPathsAreSecuritySensitive(t *testing.T) {
	for _, path := range []string{"GEMINI.md", "docs/GEMINI.md", ".gemini/settings.json", "nested/.gemini/policies/review.toml"} {
		if !hasSecuritySensitivePath([]string{path}, nil) {
			t.Errorf("Gemini control path %q was not security sensitive", path)
		}
	}
	if hasSecuritySensitivePath([]string{"src/gemini.go"}, nil) {
		t.Fatal("ordinary Gemini source file was classified as a control path")
	}
}

func TestAutoFixRetryPreservesGeminiModel(t *testing.T) {
	cfg := config.Defaults()
	preserveRetryReviewerSettings(&cfg, model.Manifest{Reviewers: []model.ReviewerResult{
		{Reviewer: "gemini", Model: "saved-gemini-model"},
	}}, map[string]bool{"gemini": true})
	if !cfg.Reviewers.Gemini.Enabled || cfg.Reviewers.Gemini.Model != "saved-gemini-model" {
		t.Fatalf("saved Gemini settings = %#v", cfg.Reviewers.Gemini)
	}
}
