package config

import "github.com/herikwebb/cora/internal/model"

// SnapshotReviewPolicy captures the effective, profile-expanded review policy
// used for a run. The snapshot is safe to persist in review and auto-fix
// manifests and can later prove that an exact-diff approval was obtained under
// policy equivalent to a resumed loop.
func SnapshotReviewPolicy(cfg Config) model.AutoFixReviewPolicy {
	checks := make([]model.AutoFixCheckPolicy, 0, len(cfg.Checks))
	for _, check := range cfg.Checks {
		checks = append(checks, model.AutoFixCheckPolicy{
			Name: check.Name, Command: append([]string(nil), check.Command...), Timeout: model.NewDuration(check.Timeout.Duration),
			EnvAllowlist: append([]string(nil), check.EnvAllowlist...), Profile: check.Profile,
		})
	}
	return model.AutoFixReviewPolicy{
		ReviewerTimeout: model.NewDuration(cfg.ReviewerTimeout.Duration), OverallTimeout: model.NewDuration(cfg.OverallTimeout.Duration),
		QueueTimeout: model.NewDuration(cfg.QueueTimeout.Duration), StrictPolicy: cfg.StrictPolicy,
		CrossExamineBlockingFindings: cfg.CrossExamineBlockingFindings, RequireCleanTree: cfg.RequireCleanTree,
		AllowAPIBilling: cfg.AllowAPIBilling, AllowUnsafeChecks: cfg.AllowUnsafeChecks,
		MinimumApprovals: cfg.MinimumApprovals, BlockingSeverities: append([]string(nil), cfg.BlockingSeverities...),
		PromptFile: cfg.PromptFile, Codex: snapshotReviewerPolicy(cfg.Reviewers.Codex), Claude: snapshotReviewerPolicy(cfg.Reviewers.Claude),
		Escalation: model.AutoFixEscalationPolicy{
			Enabled: cfg.Escalation.Enabled, Model: cfg.Escalation.Model, Effort: cfg.Escalation.Effort,
			MaxTurns: clonePolicyInt(cfg.Escalation.MaxTurns), MaxBudgetUSD: clonePolicyFloat(cfg.Escalation.MaxBudgetUSD),
			SecurityPathMarkers:    append([]string(nil), cfg.Escalation.SecurityPathMarkers...),
			ForceSecuritySensitive: cfg.Escalation.ForceSecuritySensitive, AdjudicateDisagreements: cfg.Escalation.AdjudicateDisagreements,
		},
		CrossExamination: model.AutoFixCrossExaminationPolicy{
			Timeout: model.NewDuration(cfg.CrossExamination.Timeout.Duration), MaxTurns: cfg.CrossExamination.MaxTurns,
			MaxBudgetUSD: cfg.CrossExamination.MaxBudgetUSD,
		},
		Checks: checks,
	}
}

// ApplyReviewPolicy restores a previously captured effective policy onto a
// freshly loaded trusted-base configuration. Effective checks are already
// profile-expanded, so later profile/default changes cannot alter a retry or
// resumed auto-fix loop.
func ApplyReviewPolicy(cfg Config, policy model.AutoFixReviewPolicy) (Config, error) {
	cfg.ReviewerTimeout = Duration{Duration: policy.ReviewerTimeout.Duration}
	cfg.OverallTimeout = Duration{Duration: policy.OverallTimeout.Duration}
	cfg.QueueTimeout = Duration{Duration: policy.QueueTimeout.Duration}
	cfg.StrictPolicy = policy.StrictPolicy
	cfg.CrossExamineBlockingFindings = policy.CrossExamineBlockingFindings
	cfg.RequireCleanTree = policy.RequireCleanTree
	cfg.AllowAPIBilling = policy.AllowAPIBilling
	cfg.AllowUnsafeChecks = policy.AllowUnsafeChecks
	cfg.MinimumApprovals = policy.MinimumApprovals
	cfg.BlockingSeverities = append([]string(nil), policy.BlockingSeverities...)
	cfg.PromptFile = policy.PromptFile
	cfg.Reviewers.Codex = restorePolicyReviewer(policy.Codex)
	cfg.Reviewers.Claude = restorePolicyReviewer(policy.Claude)
	cfg.Escalation = Escalation{
		Enabled: policy.Escalation.Enabled, Model: policy.Escalation.Model, Effort: policy.Escalation.Effort,
		MaxTurns: clonePolicyInt(policy.Escalation.MaxTurns), MaxBudgetUSD: clonePolicyFloat(policy.Escalation.MaxBudgetUSD),
		SecurityPathMarkers:     append([]string(nil), policy.Escalation.SecurityPathMarkers...),
		ForceSecuritySensitive:  policy.Escalation.ForceSecuritySensitive,
		AdjudicateDisagreements: policy.Escalation.AdjudicateDisagreements,
	}
	cfg.CrossExamination = CrossExamination{
		Timeout: Duration{Duration: policy.CrossExamination.Timeout.Duration}, MaxTurns: policy.CrossExamination.MaxTurns,
		MaxBudgetUSD: policy.CrossExamination.MaxBudgetUSD,
	}
	cfg.Checks = make([]Check, 0, len(policy.Checks))
	for _, check := range policy.Checks {
		cfg.Checks = append(cfg.Checks, Check{
			Name: check.Name, Command: append([]string(nil), check.Command...), Timeout: Duration{Duration: check.Timeout.Duration},
			EnvAllowlist: append([]string(nil), check.EnvAllowlist...), Profile: check.Profile,
		})
	}
	cfg.ValidationProfiles = nil
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func restorePolicyReviewer(policy model.AutoFixReviewerPolicy) Reviewer {
	return Reviewer{
		Enabled: policy.Enabled, Command: policy.Command, Model: policy.Model, Effort: policy.Effort,
		MaxTurns: policy.MaxTurns, FinalizationTurns: policy.FinalizationTurns,
		MaxBudgetUSD: policy.MaxBudgetUSD, MaxConcurrency: policy.MaxConcurrency,
	}
}

func snapshotReviewerPolicy(reviewer Reviewer) model.AutoFixReviewerPolicy {
	return model.AutoFixReviewerPolicy{
		Enabled: reviewer.Enabled, Command: reviewer.Command, Model: reviewer.Model, Effort: reviewer.Effort,
		MaxTurns: reviewer.MaxTurns, FinalizationTurns: reviewer.FinalizationTurns,
		MaxBudgetUSD: reviewer.MaxBudgetUSD, MaxConcurrency: reviewer.MaxConcurrency,
	}
}

func clonePolicyInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func clonePolicyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
