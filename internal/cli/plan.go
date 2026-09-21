package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/orchestrator"
	"github.com/herikwebb/cora/internal/record"
	"github.com/herikwebb/cora/internal/webevidence"
	"github.com/spf13/cobra"
)

type planOptions struct {
	Base               string
	Commit             string
	Range              string
	Uncommitted        bool
	Parent             int
	AllowAPIBilling    bool
	AllowReviewWeb     bool
	AllowUnsafeChecks  bool
	SecuritySensitive  bool
	Adjudicate         bool
	Strict             bool
	Profiles           []string
	ValidationEvidence []string
	WebEvidence        []string
}

type reviewPolicyOverrides struct {
	AllowAPIBilling   bool
	AllowReviewWeb    bool
	AllowUnsafeChecks bool
	SecuritySensitive bool
	Adjudicate        bool
	Strict            bool
}

type reviewPlan struct {
	SchemaVersion      string                 `json:"schema_version"`
	Repository         string                 `json:"repository"`
	RepositoryIdentity string                 `json:"repository_identity"`
	ConfigSources      []string               `json:"config_sources"`
	Target             model.Target           `json:"target"`
	ChangedPaths       []string               `json:"changed_paths"`
	Policy             planPolicy             `json:"policy"`
	Reviewers          []planReviewer         `json:"reviewers"`
	Security           planSecurity           `json:"security"`
	Validation         planValidation         `json:"validation"`
	WebEvidence        planWebEvidence        `json:"web_evidence"`
	Capacity           []planProviderCapacity `json:"capacity"`
	Ready              bool                   `json:"ready"`
	BlockingIssues     []string               `json:"blocking_issues,omitempty"`
	Warnings           []string               `json:"warnings,omitempty"`
}

type planPolicy struct {
	MinimumApprovals   int            `json:"minimum_approvals"`
	BlockingSeverities []string       `json:"blocking_severities"`
	Strict             bool           `json:"strict"`
	OverallTimeout     model.Duration `json:"overall_timeout_ms"`
	QueueTimeout       model.Duration `json:"queue_timeout_ms"`
	RequireCleanTree   bool           `json:"require_clean_tree"`
	AllowAPIBilling    bool           `json:"allow_api_billing"`
	AllowReviewWeb     bool           `json:"allow_review_web"`
	PromptSource       string         `json:"prompt_source"`
}

type planReviewer struct {
	Name              string         `json:"name"`
	Provider          string         `json:"provider"`
	Phase             string         `json:"phase"`
	Enabled           bool           `json:"enabled"`
	Scheduled         bool           `json:"scheduled"`
	Conditional       bool           `json:"conditional"`
	Required          bool           `json:"required"`
	Condition         string         `json:"condition,omitempty"`
	Command           string         `json:"command"`
	Model             string         `json:"model,omitempty"`
	ModelSource       string         `json:"model_source"`
	Effort            string         `json:"effort,omitempty"`
	Timeout           model.Duration `json:"timeout_ms"`
	MaxTurns          int            `json:"max_turns,omitempty"`
	FinalizationTurns int            `json:"finalization_turns,omitempty"`
	MaxBudgetUSD      float64        `json:"max_budget_usd"`
	MaxConcurrency    int            `json:"max_concurrency"`
}

type planSecurity struct {
	Enabled           bool     `json:"enabled"`
	Triggered         bool     `json:"triggered"`
	Forced            bool     `json:"forced"`
	Triggers          []string `json:"triggers,omitempty"`
	ControlFiles      []string `json:"control_files,omitempty"`
	SensitivePaths    []string `json:"sensitive_paths,omitempty"`
	PathMarkers       []string `json:"path_markers"`
	ReviewerAvailable bool     `json:"reviewer_available"`
	Model             string   `json:"model,omitempty"`
	Effort            string   `json:"effort,omitempty"`
}

type planValidation struct {
	Status                  string                 `json:"status"`
	SelectedProfiles        []string               `json:"selected_profiles"`
	Checks                  []planCheck            `json:"checks"`
	ImportedEvidence        []planImportedEvidence `json:"imported_evidence"`
	StrictlyRequired        bool                   `json:"strictly_required"`
	HostExecutionAuthorized bool                   `json:"host_execution_authorized"`
	Isolation               string                 `json:"isolation"`
}

type planWebEvidence struct {
	Status         string   `json:"status"`
	Authorized     bool     `json:"authorized"`
	URLs           []string `json:"urls"`
	MaximumSources int      `json:"maximum_sources"`
	PerSourceBytes int      `json:"per_source_bytes"`
	TotalBytes     int      `json:"total_bytes"`
	Isolation      string   `json:"isolation"`
}

type planImportedEvidence struct {
	SourcePath  string                            `json:"source_path"`
	CheckName   string                            `json:"check_name"`
	Status      string                            `json:"status"`
	Isolation   string                            `json:"isolation"`
	Attestation *model.ImportedValidationEvidence `json:"attestation"`
}

type planCheck struct {
	Name         string         `json:"name"`
	Profile      string         `json:"profile,omitempty"`
	Command      []string       `json:"command"`
	Timeout      model.Duration `json:"timeout_ms"`
	EnvAllowlist []string       `json:"env_allowlist,omitempty"`
}

type planProviderCapacity struct {
	Provider            string         `json:"provider"`
	MaxConcurrency      int            `json:"max_concurrency"`
	InitialDemand       int            `json:"initial_demand"`
	TargetedDemand      int            `json:"targeted_demand"`
	ConditionalRoles    []string       `json:"conditional_roles,omitempty"`
	QueueTimeout        model.Duration `json:"queue_timeout_ms"`
	Scope               string         `json:"scope"`
	CurrentAvailability string         `json:"current_availability"`
}

func newPlanCommand(opts *options) *cobra.Command {
	var input planOptions
	command := &cobra.Command{
		Use:   "plan",
		Short: "Show the effective review plan without starting a run",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			repo, err := gitx.Discover(command.Context(), opts.repo)
			if err != nil {
				return err
			}
			personal, err := config.LoadPersonal()
			if err != nil {
				return err
			}
			plan, err := buildReviewPlan(command.Context(), repo, personal, input)
			if err != nil {
				return err
			}
			if opts.json {
				return writePlanJSON(command.OutOrStdout(), plan)
			}
			printReviewPlan(command.OutOrStdout(), plan)
			return nil
		},
	}
	command.Flags().StringVar(&input.Base, "base", "", "base branch or commit")
	command.Flags().StringVar(&input.Commit, "commit", "", "plan review of one commit")
	command.Flags().StringVar(&input.Range, "range", "", "plan review of BASE..HEAD")
	command.Flags().BoolVar(&input.Uncommitted, "uncommitted", false, "plan review of working-tree changes")
	command.Flags().IntVar(&input.Parent, "parent", 0, "parent number for a merge commit")
	command.Flags().BoolVar(&input.AllowAPIBilling, "allow-api-billing", false, "plan separately billed provider authentication")
	command.Flags().BoolVar(&input.AllowReviewWeb, "allow-review-web", false, "authorize planned capture of explicitly selected HTTPS evidence")
	command.Flags().BoolVar(&input.AllowUnsafeChecks, "allow-unsafe-checks", false, "authorize planned host validation checks")
	command.Flags().BoolVar(&input.SecuritySensitive, "security-sensitive", false, "force the targeted security review in the plan")
	command.Flags().BoolVar(&input.Adjudicate, "adjudicate", false, "enable conditional disagreement adjudication")
	command.Flags().BoolVar(&input.Strict, "strict", false, "treat minor findings as blocking and require validation checks")
	command.Flags().StringSliceVar(&input.Profiles, "profile", nil, "validation profile to plan (repeatable; auto detects a built-in profile)")
	command.Flags().StringArrayVar(&input.ValidationEvidence, "validation-evidence", nil, "inspect external validation evidence bound to this exact diff (repeatable)")
	command.Flags().StringArrayVar(&input.WebEvidence, "web-evidence", nil, "validate an HTTPS evidence URL without fetching it (repeatable)")
	return command
}

func buildReviewPlan(ctx context.Context, repo gitx.Repo, personal config.Config, input planOptions) (reviewPlan, error) {
	candidateBase := personal.Base
	if input.Base != "" {
		candidateBase = input.Base
	}
	resolve := func(baseRef string) (model.Target, error) {
		return repo.ResolveTarget(ctx, gitx.TargetOptions{
			Base: baseRef, Commit: input.Commit, Range: input.Range,
			Uncommitted: input.Uncommitted, Parent: input.Parent,
		})
	}
	target, err := resolve(candidateBase)
	if err != nil {
		return reviewPlan{}, err
	}
	cfg, err := loadTrustedConfig(ctx, repo, personal, target)
	if err != nil {
		return reviewPlan{}, err
	}
	if target.Mode == "branch" && input.Base == "" && cfg.Base != "" && cfg.Base != target.BaseRef {
		candidateBase = cfg.Base
		target, err = resolve(candidateBase)
		if err != nil {
			return reviewPlan{}, err
		}
		cfg, err = loadTrustedConfig(ctx, repo, personal, target)
		if err != nil {
			return reviewPlan{}, err
		}
		if cfg.Base != "" && cfg.Base != candidateBase {
			return reviewPlan{}, fmt.Errorf("trusted repository config changes base from %q to %q; pass --base explicitly", candidateBase, cfg.Base)
		}
	}
	if target.Mode == "branch" {
		candidateBase = target.BaseRef
		if input.Base != "" {
			candidateBase = input.Base
		}
		cfg.Base = candidateBase
	}
	applyReviewPolicyOverrides(&cfg, reviewPolicyOverrides{
		AllowAPIBilling: input.AllowAPIBilling, AllowReviewWeb: input.AllowReviewWeb, AllowUnsafeChecks: input.AllowUnsafeChecks,
		SecuritySensitive: input.SecuritySensitive, Adjudicate: input.Adjudicate, Strict: input.Strict,
	})
	if err := cfg.Validate(); err != nil {
		return reviewPlan{}, err
	}
	profiles := append([]string(nil), input.Profiles...)
	if len(profiles) == 0 && len(cfg.Checks) == 0 && cfg.AllowUnsafeChecks {
		profiles = []string{"auto"}
	}
	profiles, err = expandAutoProfiles(ctx, repo, target, profiles)
	if err != nil {
		return reviewPlan{}, err
	}
	cfg, err = config.ApplyProfiles(cfg, profiles)
	if err != nil {
		return reviewPlan{}, err
	}
	changedPaths, err := repo.ChangedPaths(ctx, target)
	if err != nil {
		return reviewPlan{}, err
	}
	repositoryIdentity, err := repo.StableIdentity(ctx)
	if err != nil {
		return reviewPlan{}, err
	}
	importedEvidence, err := inspectPlannedValidationEvidence(input.ValidationEvidence, cfg.Checks, target, repositoryIdentity)
	if err != nil {
		return reviewPlan{}, err
	}
	webEvidenceURLs, err := webevidence.ValidateURLs(input.WebEvidence)
	if err != nil {
		return reviewPlan{}, err
	}
	controlFiles, sensitivePaths := orchestrator.AnalyzeSecurityPaths(changedPaths, cfg.Escalation.SecurityPathMarkers)
	promptSource, err := orchestrator.ResolveReviewPromptSource(ctx, repo, cfg, target.BaseSHA)
	if err != nil {
		return reviewPlan{}, err
	}
	securityTriggered := cfg.Escalation.Enabled && (cfg.Escalation.ForceSecuritySensitive || len(sensitivePaths) > 0)
	triggers := make([]string, 0, 2)
	if cfg.Escalation.ForceSecuritySensitive {
		triggers = append(triggers, "forced")
	}
	if len(sensitivePaths) > 0 {
		triggers = append(triggers, "sensitive_path")
	}

	plan := reviewPlan{
		SchemaVersion: model.SchemaVersion, Repository: repo.Root, RepositoryIdentity: repositoryIdentity,
		ConfigSources: append([]string(nil), cfg.LoadedFiles...), Target: target,
		ChangedPaths: append([]string(nil), changedPaths...),
		Policy: planPolicy{
			MinimumApprovals: cfg.MinimumApprovals, BlockingSeverities: planBlockingSeverities(cfg), Strict: cfg.StrictPolicy,
			OverallTimeout: model.NewDuration(cfg.OverallTimeout.Duration), QueueTimeout: model.NewDuration(cfg.QueueTimeout.Duration),
			RequireCleanTree: cfg.RequireCleanTree, AllowAPIBilling: cfg.AllowAPIBilling, AllowReviewWeb: cfg.AllowReviewWeb,
			PromptSource: promptSource,
		},
		Security: planSecurity{
			Enabled: cfg.Escalation.Enabled, Triggered: securityTriggered, Forced: cfg.Escalation.ForceSecuritySensitive,
			Triggers: triggers, ControlFiles: controlFiles, SensitivePaths: sensitivePaths,
			PathMarkers:       append([]string(nil), cfg.Escalation.SecurityPathMarkers...),
			ReviewerAvailable: cfg.Reviewers.Claude.Enabled, Model: cfg.Escalation.Model, Effort: cfg.Escalation.Effort,
		},
		Validation:  plannedValidation(cfg, profiles, importedEvidence),
		WebEvidence: plannedWebEvidence(cfg, webEvidenceURLs),
	}
	plan.Reviewers = plannedReviewers(cfg, securityTriggered)
	plan.Capacity = plannedCapacity(cfg, plan.Reviewers)
	plan.BlockingIssues, plan.Warnings = planPreflight(cfg, target, securityTriggered, len(importedEvidence), len(webEvidenceURLs))
	plan.Ready = len(plan.BlockingIssues) == 0
	return plan, nil
}

func applyReviewPolicyOverrides(cfg *config.Config, overrides reviewPolicyOverrides) {
	if overrides.AllowAPIBilling {
		cfg.AllowAPIBilling = true
	}
	if overrides.AllowReviewWeb {
		cfg.AllowReviewWeb = true
	}
	if overrides.AllowUnsafeChecks {
		cfg.AllowUnsafeChecks = true
	}
	if overrides.SecuritySensitive {
		cfg.Escalation.Enabled = true
		cfg.Escalation.ForceSecuritySensitive = true
	}
	if overrides.Adjudicate {
		cfg.Escalation.Enabled = true
		cfg.Escalation.AdjudicateDisagreements = true
	}
	if overrides.Strict {
		cfg.StrictPolicy = true
	}
}

func planBlockingSeverities(cfg config.Config) []string {
	severities := append([]string(nil), cfg.BlockingSeverities...)
	if cfg.StrictPolicy && !containsString(severities, "minor") {
		severities = append(severities, "minor")
	}
	return severities
}

func plannedReviewers(cfg config.Config, securityTriggered bool) []planReviewer {
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	escalation := cfg.Reviewers.Claude
	escalation.Model = cfg.Escalation.Model
	escalation.Effort = cfg.Escalation.Effort
	if cfg.Escalation.MaxTurns != nil {
		escalation.MaxTurns = *cfg.Escalation.MaxTurns
	}
	if cfg.Escalation.MaxBudgetUSD != nil {
		escalation.MaxBudgetUSD = *cfg.Escalation.MaxBudgetUSD
	}
	cross := escalation
	cross.MaxTurns = cfg.CrossExamination.MaxTurns
	cross.MaxBudgetUSD = cfg.CrossExamination.MaxBudgetUSD

	reviewers := []planReviewer{
		plannedReviewer("codex", "codex", "initial", cfg.Reviewers.Codex, limits["codex"], cfg.Reviewers.Codex.Enabled, false, cfg.Reviewers.Codex.Enabled, ""),
		plannedReviewer("claude", "claude", "initial", cfg.Reviewers.Claude, limits["claude"], cfg.Reviewers.Claude.Enabled, false, cfg.Reviewers.Claude.Enabled, ""),
		plannedReviewer("claude-security", "claude", "security-review", escalation, limits["claude-security"], cfg.Escalation.Enabled && cfg.Reviewers.Claude.Enabled, false, securityTriggered, "security-sensitive paths are present or explicitly forced"),
		plannedReviewer("claude-escalation", "claude", "dispute-adjudication", escalation, limits["claude-escalation"], cfg.Escalation.Enabled && cfg.Escalation.AdjudicateDisagreements && cfg.Reviewers.Claude.Enabled, true, false, "ordinary reviewers disagree while approval remains possible"),
		plannedReviewer("claude-cross-examination", "claude", "cross-examination", cross, limits["claude-cross-examination"], cfg.CrossExamineBlockingFindings && cfg.Reviewers.Claude.Enabled, true, false, "an uncorroborated blocking finding could determine the outcome"),
	}
	return reviewers
}

func plannedReviewer(name, providerName, phase string, reviewer config.Reviewer, limit model.ReviewerExecutionLimit, enabled, conditional, required bool, condition string) planReviewer {
	modelSource := "configured"
	if strings.TrimSpace(reviewer.Model) == "" {
		modelSource = "provider-default"
	}
	return planReviewer{
		Name: name, Provider: providerName, Phase: phase, Enabled: enabled,
		Scheduled: required && enabled, Conditional: conditional, Required: required,
		Condition: condition, Command: reviewer.Command, Model: reviewer.Model, ModelSource: modelSource,
		Effort: reviewer.Effort, Timeout: limit.Timeout, MaxTurns: limit.MaxTurns,
		FinalizationTurns: claudeFinalizationTurns(providerName, reviewer.FinalizationTurns),
		MaxBudgetUSD:      reviewer.MaxBudgetUSD, MaxConcurrency: reviewer.MaxConcurrency,
	}
}

func claudeFinalizationTurns(providerName string, turns int) int {
	if providerName == "claude" {
		return turns
	}
	return 0
}

func plannedValidation(cfg config.Config, profiles []string, imported []planImportedEvidence) planValidation {
	checks := make([]planCheck, 0, len(cfg.Checks))
	for _, check := range cfg.Checks {
		checks = append(checks, planCheck{
			Name: check.Name, Profile: check.Profile, Command: append([]string(nil), check.Command...),
			Timeout: model.NewDuration(check.Timeout.Duration), EnvAllowlist: append([]string(nil), check.EnvAllowlist...),
		})
	}
	status := "not_configured"
	switch {
	case len(checks) > 0 && !cfg.AllowUnsafeChecks:
		status = "authorization_required"
	case len(checks) > 0 && len(imported) > 0:
		status = "planned_with_imported_evidence"
	case len(checks) > 0:
		status = "planned"
	case len(imported) > 0:
		status = "imported_evidence_validated"
	}
	isolation := "no configured check execution"
	if len(checks) > 0 {
		isolation = "disposable remote-free clone, minimal environment; host execution is not sandboxed"
	}
	return planValidation{
		Status: status, SelectedProfiles: append([]string(nil), profiles...), Checks: checks, ImportedEvidence: imported,
		StrictlyRequired: cfg.StrictPolicy, HostExecutionAuthorized: cfg.AllowUnsafeChecks,
		Isolation: isolation,
	}
}

func plannedWebEvidence(cfg config.Config, urls []string) planWebEvidence {
	status := "not_requested"
	if len(urls) > 0 && !cfg.AllowReviewWeb {
		status = "authorization_required"
	} else if len(urls) > 0 {
		status = "planned"
	}
	return planWebEvidence{
		Status: status, Authorized: cfg.AllowReviewWeb, URLs: append([]string(nil), urls...),
		MaximumSources: webevidence.MaxSources, PerSourceBytes: webevidence.MaxSourceBytes, TotalBytes: webevidence.MaxTotalBytes,
		Isolation: "Cora captures once; reviewers, tests, and shell commands remain network-disabled",
	}
}

func inspectPlannedValidationEvidence(paths []string, configured []config.Check, target model.Target, repositoryIdentity string) ([]planImportedEvidence, error) {
	names := make(map[string]bool, len(configured)+len(paths))
	for _, check := range configured {
		names[check.Name] = true
	}
	results := make([]planImportedEvidence, 0, len(paths))
	for _, path := range paths {
		result, err := record.InspectValidationEvidence(path, target, repositoryIdentity)
		if err != nil {
			return nil, err
		}
		if names[result.Name] {
			return nil, fmt.Errorf("validation check name %q is duplicated by imported evidence", result.Name)
		}
		names[result.Name] = true
		results = append(results, planImportedEvidence{
			SourcePath: path, CheckName: result.Name, Status: result.Status,
			Isolation: result.Isolation, Attestation: result.ImportedEvidence,
		})
	}
	return results, nil
}

func plannedCapacity(cfg config.Config, reviewers []planReviewer) []planProviderCapacity {
	type demand struct {
		initial, targeted int
		conditional       []string
	}
	demands := map[string]*demand{"codex": {}, "claude": {}}
	for _, reviewer := range reviewers {
		if !reviewer.Enabled {
			continue
		}
		entry := demands[reviewer.Provider]
		switch {
		case reviewer.Phase == "initial" && reviewer.Scheduled:
			entry.initial++
		case reviewer.Scheduled:
			entry.targeted++
		case reviewer.Conditional:
			entry.conditional = append(entry.conditional, reviewer.Name)
		}
	}
	capacities := make([]planProviderCapacity, 0, 2)
	for _, providerName := range []string{"codex", "claude"} {
		entry := demands[providerName]
		limit := cfg.Reviewers.Codex.MaxConcurrency
		if providerName == "claude" {
			limit = cfg.Reviewers.Claude.MaxConcurrency
		}
		if entry.initial == 0 && entry.targeted == 0 && len(entry.conditional) == 0 {
			continue
		}
		capacities = append(capacities, planProviderCapacity{
			Provider: providerName, MaxConcurrency: limit, InitialDemand: entry.initial, TargetedDemand: entry.targeted,
			ConditionalRoles: append([]string(nil), entry.conditional...), QueueTimeout: model.NewDuration(cfg.QueueTimeout.Duration),
			Scope: "user-global FIFO across Cora processes", CurrentAvailability: "unknown until provider-slot acquisition",
		})
	}
	return capacities
}

func planPreflight(cfg config.Config, target model.Target, securityTriggered bool, importedEvidence, webEvidence int) (blocking, warnings []string) {
	if cfg.RequireCleanTree && target.Dirty && target.Mode != "uncommitted" {
		blocking = append(blocking, "working tree is not clean; commit or stash changes, or plan --uncommitted")
	}
	if len(cfg.Checks) > 0 && !cfg.AllowUnsafeChecks {
		blocking = append(blocking, "configured checks require --allow-unsafe-checks or allow_unsafe_host_checks = true")
	}
	if cfg.StrictPolicy && len(cfg.Checks) == 0 && importedEvidence == 0 {
		blocking = append(blocking, "strict policy requires at least one validation check")
	}
	if securityTriggered && !cfg.Reviewers.Claude.Enabled {
		blocking = append(blocking, "a targeted security review is required, but Claude is disabled")
	}
	if webEvidence > 0 && !cfg.AllowReviewWeb {
		blocking = append(blocking, "web evidence requires --allow-review-web or allow_review_web = true")
	}
	if len(cfg.Checks) == 0 && importedEvidence == 0 && !cfg.StrictPolicy {
		warnings = append(warnings, "no validation check is configured; validation_status will be not_run")
	}
	if !target.Finalizable {
		warnings = append(warnings, "this target is not finalizable and cannot produce a reusable exact-diff approval")
	}
	return blocking, warnings
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func writePlanJSON(writer io.Writer, plan reviewPlan) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(plan)
}

func printReviewPlan(writer io.Writer, plan reviewPlan) {
	ready := "yes"
	if !plan.Ready {
		ready = "no"
	}
	fmt.Fprintln(writer, "CORA REVIEW PLAN")
	fmt.Fprintf(writer, "Repository: %s (%s)\n", plan.Repository, plan.RepositoryIdentity)
	fmt.Fprintf(writer, "Target: %s %s..%s\n", plan.Target.Mode, plan.Target.BaseRef, plan.Target.HeadRef)
	fmt.Fprintf(writer, "Base: %s\nHead: %s\nDiff: %s (%d changed paths)\n", plan.Target.BaseSHA, plan.Target.HeadSHA, plan.Target.DiffHash, len(plan.ChangedPaths))
	fmt.Fprintf(writer, "Ready: %s\n", ready)
	if len(plan.ConfigSources) == 0 {
		fmt.Fprintln(writer, "Config: built-in defaults")
	} else {
		fmt.Fprintf(writer, "Config: %s\n", strings.Join(plan.ConfigSources, ", "))
	}
	fmt.Fprintf(writer, "Policy: approvals=%d blocking=%s strict=%t clean_tree=%t api_billing=%t review_web=%t overall=%s queue=%s prompt=%s\n",
		plan.Policy.MinimumApprovals, strings.Join(plan.Policy.BlockingSeverities, ","), plan.Policy.Strict,
		plan.Policy.RequireCleanTree, plan.Policy.AllowAPIBilling, plan.Policy.AllowReviewWeb, plan.Policy.OverallTimeout.Duration, plan.Policy.QueueTimeout.Duration, plan.Policy.PromptSource)

	fmt.Fprintln(writer, "Reviewers:")
	for _, reviewer := range plan.Reviewers {
		status := "disabled"
		switch {
		case reviewer.Scheduled:
			status = "scheduled"
		case reviewer.Enabled && reviewer.Conditional:
			status = "conditional"
		case reviewer.Enabled:
			status = "not triggered"
		}
		modelName := reviewer.Model
		if modelName == "" {
			modelName = "provider default"
		}
		turns := "n/a"
		if reviewer.MaxTurns > 0 {
			turns = fmt.Sprintf("%d", reviewer.MaxTurns)
		}
		fmt.Fprintf(writer, "  %-26s %-13s command=%s model=%s effort=%s timeout=%s max_turns=%s concurrency=%d",
			reviewer.Name, status, strconv.Quote(reviewer.Command), modelName, defaultString(reviewer.Effort, "provider default"), reviewer.Timeout.Duration, turns, reviewer.MaxConcurrency)
		if reviewer.Provider == "claude" {
			fmt.Fprintf(writer, " finalization_turns=%d", reviewer.FinalizationTurns)
			if reviewer.MaxBudgetUSD > 0 {
				fmt.Fprintf(writer, " max_budget_usd=%.2f", reviewer.MaxBudgetUSD)
			} else {
				fmt.Fprint(writer, " max_budget_usd=unlimited")
			}
		}
		if reviewer.Condition != "" && !reviewer.Scheduled {
			fmt.Fprintf(writer, " (%s)", reviewer.Condition)
		}
		fmt.Fprintln(writer)
	}

	securityState := "not triggered"
	if !plan.Security.Enabled {
		securityState = "disabled"
	} else if plan.Security.Triggered {
		securityState = "triggered"
	}
	fmt.Fprintf(writer, "Security: %s model=%s effort=%s\n", securityState, defaultString(plan.Security.Model, "provider default"), defaultString(plan.Security.Effort, "provider default"))
	if len(plan.Security.Triggers) > 0 {
		fmt.Fprintf(writer, "  Triggers: %s\n", strings.Join(plan.Security.Triggers, ", "))
	}
	if len(plan.Security.SensitivePaths) > 0 {
		fmt.Fprintf(writer, "  Sensitive paths: %s\n", formatPlanArguments(plan.Security.SensitivePaths))
	}

	fmt.Fprintf(writer, "Validation: %s host_execution_authorized=%t", plan.Validation.Status, plan.Validation.HostExecutionAuthorized)
	if len(plan.Validation.SelectedProfiles) > 0 {
		fmt.Fprintf(writer, " profiles=%s", formatPlanArguments(plan.Validation.SelectedProfiles))
	}
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  Isolation: %s\n", plan.Validation.Isolation)
	for _, check := range plan.Validation.Checks {
		profile := ""
		if check.Profile != "" {
			profile = " profile=" + strconv.Quote(check.Profile)
		}
		fmt.Fprintf(writer, "  check=%s%s timeout=%s env_allowlist=%s command=%s\n",
			strconv.Quote(check.Name), profile, check.Timeout.Duration,
			formatPlanArguments(check.EnvAllowlist), formatPlanArguments(check.Command))
	}
	for _, evidence := range plan.Validation.ImportedEvidence {
		verifier, source, trust, summary := "unknown", "unknown", "unknown", "unknown"
		var command []string
		if evidence.Attestation != nil {
			verifier, source, trust = evidence.Attestation.Verifier, evidence.Attestation.Source, evidence.Attestation.Trust
			summary, command = evidence.Attestation.Summary, evidence.Attestation.Command
		}
		fmt.Fprintf(writer, "  evidence=%s status=%s isolation=%s verifier=%s source=%s trust=%s command=%s summary=%s\n",
			strconv.Quote(evidence.CheckName), strconv.Quote(evidence.Status), strconv.Quote(evidence.Isolation),
			strconv.Quote(verifier), strconv.Quote(source), strconv.Quote(trust), formatPlanArguments(command), strconv.Quote(summary))
	}

	fmt.Fprintf(writer, "Web evidence: %s authorized=%t sources=%d limit=%d bytes/source, %d bytes total\n",
		plan.WebEvidence.Status, plan.WebEvidence.Authorized, len(plan.WebEvidence.URLs),
		plan.WebEvidence.PerSourceBytes, plan.WebEvidence.TotalBytes)
	fmt.Fprintf(writer, "  Isolation: %s\n", plan.WebEvidence.Isolation)
	for _, sourceURL := range plan.WebEvidence.URLs {
		fmt.Fprintf(writer, "  url=%s\n", strconv.Quote(sourceURL))
	}

	fmt.Fprintln(writer, "Capacity:")
	for _, capacity := range plan.Capacity {
		fmt.Fprintf(writer, "  %s: global_limit=%d initial_demand=%d targeted_demand=%d queue_timeout=%s availability=%s",
			capacity.Provider, capacity.MaxConcurrency, capacity.InitialDemand, capacity.TargetedDemand, capacity.QueueTimeout.Duration, capacity.CurrentAvailability)
		if len(capacity.ConditionalRoles) > 0 {
			fmt.Fprintf(writer, " conditional=%s", strings.Join(capacity.ConditionalRoles, ","))
		}
		fmt.Fprintln(writer)
	}
	for _, issue := range plan.BlockingIssues {
		fmt.Fprintf(writer, "BLOCKED: %s\n", issue)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(writer, "WARNING: %s\n", warning)
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatPlanArguments(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = strconv.Quote(argument)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
