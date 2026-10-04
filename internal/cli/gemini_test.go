package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/record"
)

func TestRetrySelectsGeminiAndPreservesItsSavedModel(t *testing.T) {
	cfg := config.Defaults()
	manifest := model.Manifest{Reviewers: []model.ReviewerResult{
		{Reviewer: "codex", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "claude", Status: "completed", Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true}},
		{Reviewer: "gemini", Status: "incomplete", FailureKind: "quota", Model: "saved-gemini-model"},
	}}
	selected, err := selectRetryReviewers(manifest.Reviewers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, map[string]bool{"gemini": true}) {
		t.Fatalf("selected = %#v", selected)
	}
	requested, err := selectRetryReviewers(manifest.Reviewers, []string{" GEMINI "})
	if err != nil || !reflect.DeepEqual(requested, selected) {
		t.Fatalf("explicit Gemini retry = %#v, %v", requested, err)
	}
	lineage := record.ReviewerLineage{Reviewers: manifest.Reviewers, LatestReviewers: manifest.Reviewers}
	preserveRetryReviewerSettings(&cfg, manifest, lineage, selected)
	if !cfg.Reviewers.Gemini.Enabled || cfg.Reviewers.Gemini.Model != "saved-gemini-model" {
		t.Fatalf("saved Gemini settings = %#v", cfg.Reviewers.Gemini)
	}
}

func TestGeminiRetryTurnLimitDoesNotUseClaudeFinalizationReserve(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.MaxTurns = 1
	cfg.Reviewers.Claude.FinalizationTurns = 5
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	claudeLimit := limits["claude"]
	overrides, err := applyRetryLimitOverrides(&cfg, limits, map[string]bool{"gemini": true}, retryLimitOverrideInput{
		MaxTurns: 2, MaxTurnsSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits["gemini"].MaxTurns != 2 || limits["claude"] != claudeLimit || overrides == nil || overrides.MaxTurns == nil || *overrides.MaxTurns != 2 {
		t.Fatalf("Gemini retry limits = %#v, overrides = %#v", limits, overrides)
	}
	_, err = applyRetryLimitOverrides(&cfg, limits, map[string]bool{"gemini": true}, retryLimitOverrideInput{
		MaxTurns: 2, MaxTurnsSet: true,
	})
	if err == nil || !strings.Contains(err.Error(), "must raise gemini") {
		t.Fatalf("unchanged Gemini turn limit error = %v", err)
	}
}

func TestPlanIncludesGeminiRoleAndIndependentCapacity(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.Model = "planned-gemini-model"
	cfg.Reviewers.Gemini.MaxTurns = 17
	cfg.Reviewers.Gemini.MaxConcurrency = 3
	cfg.MinimumApprovals = 3
	reviewers := plannedReviewers(cfg, false)
	gemini := findPlannedReviewer(t, reviewers, "gemini")
	if !gemini.Enabled || !gemini.Required || !gemini.Scheduled || gemini.Conditional || gemini.Phase != "initial" || gemini.Provider != "gemini" || gemini.Model != "planned-gemini-model" || gemini.MaxTurns != 17 || gemini.FinalizationTurns != 0 {
		t.Fatalf("planned Gemini = %#v", gemini)
	}
	capacity := findPlannedCapacity(t, plannedCapacity(cfg, reviewers), "gemini")
	if capacity.MaxConcurrency != 3 || capacity.InitialDemand != 1 || capacity.TargetedDemand != 0 || len(capacity.ConditionalRoles) != 0 {
		t.Fatalf("Gemini capacity = %#v", capacity)
	}
	cfg.Reviewers.Gemini.Enabled = false
	reviewers = plannedReviewers(cfg, false)
	gemini = findPlannedReviewer(t, reviewers, "gemini")
	if gemini.Scheduled || gemini.Required || gemini.Enabled {
		t.Fatalf("disabled Gemini is scheduled = %#v", gemini)
	}
	for _, capacity := range plannedCapacity(cfg, reviewers) {
		if capacity.Provider == "gemini" {
			t.Fatalf("disabled Gemini consumes capacity = %#v", capacity)
		}
	}
}

func TestGeminiQuotaRetryWaitsForSavedReset(t *testing.T) {
	now := time.Now().UTC()
	retryAt := now.Add(time.Hour)
	notBefore := quotaNotBefore([]model.ReviewerResult{{Reviewer: "gemini", Retryable: true, RetryAt: &retryAt}}, map[string]bool{"gemini": true}, now, now)
	if !notBefore["gemini"].Equal(retryAt) {
		t.Fatalf("Gemini retry starts at %s, want %s", notBefore["gemini"], retryAt)
	}
}
