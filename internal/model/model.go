package model

import (
	"encoding/json"
	"time"
)

const (
	SchemaVersion = "1"

	StateApproved         = "approved"
	StateChangesRequested = "changes_requested"
	StateNeedsHuman       = "needs_human"
	StateIncomplete       = "incomplete"
	StateStale            = "stale"
	StatePaused           = "paused"
)

type Target struct {
	Mode        string `json:"mode"`
	BaseRef     string `json:"base_ref,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	BaseSHA     string `json:"base_sha"`
	HeadSHA     string `json:"head_sha"`
	DiffHash    string `json:"diff_hash"`
	Dirty       bool   `json:"dirty"`
	Finalizable bool   `json:"finalizable"`
}

type Finding struct {
	ID           string        `json:"id"`
	Severity     string        `json:"severity"`
	Confidence   float64       `json:"confidence"`
	File         string        `json:"file"`
	Line         int           `json:"line"`
	Claim        string        `json:"claim"`
	Evidence     string        `json:"evidence"`
	SuggestedFix string        `json:"suggested_fix"`
	Disposition  string        `json:"disposition,omitempty"`
	Reachability *Reachability `json:"reachability,omitempty"`
}

// MarshalJSON keeps normalized reports compatible with the provider schema:
// Codex requires every object property to be present, so optional finding
// fields are represented as explicit nulls instead of being omitted.
func (f Finding) MarshalJSON() ([]byte, error) {
	var disposition *string
	if f.Disposition != "" {
		value := f.Disposition
		disposition = &value
	}
	type wireFinding struct {
		ID           string        `json:"id"`
		Severity     string        `json:"severity"`
		Confidence   float64       `json:"confidence"`
		File         string        `json:"file"`
		Line         int           `json:"line"`
		Claim        string        `json:"claim"`
		Evidence     string        `json:"evidence"`
		SuggestedFix string        `json:"suggested_fix"`
		Disposition  *string       `json:"disposition"`
		Reachability *Reachability `json:"reachability"`
	}
	return json.Marshal(wireFinding{
		ID: f.ID, Severity: f.Severity, Confidence: f.Confidence, File: f.File,
		Line: f.Line, Claim: f.Claim, Evidence: f.Evidence, SuggestedFix: f.SuggestedFix,
		Disposition: disposition, Reachability: f.Reachability,
	})
}

type Reachability struct {
	Status        string   `json:"status"`
	Trigger       string   `json:"trigger"`
	Path          []string `json:"path"`
	Impact        string   `json:"impact"`
	Preconditions []string `json:"preconditions"`
}

type ConsolidatedFinding struct {
	ID                   string        `json:"id"`
	Severity             string        `json:"severity"`
	Confidence           float64       `json:"confidence"`
	File                 string        `json:"file"`
	Line                 int           `json:"line"`
	Claim                string        `json:"claim"`
	Evidence             []string      `json:"evidence"`
	SuggestedFixes       []string      `json:"suggested_fixes"`
	Reviewers            []string      `json:"reviewers"`
	SourceIDs            []string      `json:"source_ids"`
	OriginalSeverity     string        `json:"original_severity,omitempty"`
	Disposition          string        `json:"disposition,omitempty"`
	CrossExaminer        string        `json:"cross_examiner,omitempty"`
	Reachability         *Reachability `json:"reachability,omitempty"`
	CarriedFromRunIDs    []string      `json:"carried_from_run_ids,omitempty"`
	HistoricalFindingIDs []string      `json:"historical_finding_ids,omitempty"`
}

type CrossExamination struct {
	FindingID         string        `json:"finding_id"`
	Reviewer          string        `json:"reviewer"`
	Status            string        `json:"status"`
	Disposition       string        `json:"disposition,omitempty"`
	OriginalSeverity  string        `json:"original_severity"`
	EffectiveSeverity string        `json:"effective_severity,omitempty"`
	Rationale         string        `json:"rationale,omitempty"`
	Reachability      *Reachability `json:"reachability,omitempty"`
	Error             string        `json:"error,omitempty"`
}

type ReviewReport struct {
	SchemaVersion   string    `json:"schema_version"`
	Reviewer        string    `json:"reviewer,omitempty"`
	BaseSHA         string    `json:"base_sha,omitempty"`
	HeadSHA         string    `json:"head_sha,omitempty"`
	Verdict         string    `json:"verdict"`
	ContextComplete bool      `json:"context_complete"`
	Summary         string    `json:"summary"`
	Findings        []Finding `json:"findings"`
	ReviewedPaths   []string  `json:"reviewed_paths"`
	OmittedPaths    []string  `json:"omitted_paths"`
	ResidualRisks   []string  `json:"residual_risks"`
}

type ReviewerResult struct {
	Reviewer          string        `json:"reviewer"`
	Status            string        `json:"status"`
	Duration          Duration      `json:"duration_ms"`
	QueueDuration     Duration      `json:"queue_duration_ms"`
	ExecutionDuration Duration      `json:"execution_duration_ms"`
	Report            *ReviewReport `json:"report,omitempty"`
	Error             string        `json:"error,omitempty"`
	ExitCode          int           `json:"exit_code,omitempty"`
	Auth              string        `json:"auth,omitempty"`
	Tool              string        `json:"tool,omitempty"`
	ToolVersion       string        `json:"tool_version,omitempty"`
	Model             string        `json:"model,omitempty"`
	ModelSource       string        `json:"model_source,omitempty"`
	Effort            string        `json:"effort,omitempty"`
	EscalationCause   string        `json:"escalation_cause,omitempty"`
	Attempt           int           `json:"attempt"`
	ReusedFromRunID   string        `json:"reused_from_run_id,omitempty"`
	FailureKind       string        `json:"failure_kind,omitempty"`
	Retryable         bool          `json:"retryable,omitempty"`
	RetryAt           *time.Time    `json:"retry_at,omitempty"`
	Usage             Usage         `json:"usage"`
}

// Duration is encoded as milliseconds so JSON never exposes Go's nanosecond
// representation. Its embedded value remains convenient for human formatting.
type Duration struct {
	time.Duration
}

func NewDuration(value time.Duration) Duration { return Duration{Duration: value} }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Milliseconds())
}

func (d *Duration) UnmarshalJSON(contents []byte) error {
	var milliseconds int64
	if err := json.Unmarshal(contents, &milliseconds); err != nil {
		return err
	}
	d.Duration = time.Duration(milliseconds) * time.Millisecond
	return nil
}

// Usage records observable reviewer consumption. Known flags distinguish a
// genuine zero from telemetry that the provider CLI did not expose.
type Usage struct {
	Turns                    int     `json:"turns"`
	TurnsKnown               bool    `json:"turns_known"`
	TurnsPartial             bool    `json:"turns_partial,omitempty"`
	InputTokens              int64   `json:"input_tokens"`
	CachedInputTokens        int64   `json:"cached_input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	ThinkingTokens           int64   `json:"thinking_tokens"`
	ThinkingTokensKnown      bool    `json:"thinking_tokens_known"`
	ThinkingTokensPartial    bool    `json:"thinking_tokens_partial,omitempty"`
	APIEquivalentCostUSD     float64 `json:"api_equivalent_cost_usd"`
	APIEquivalentCostKnown   bool    `json:"api_equivalent_cost_known"`
	APIEquivalentCostPartial bool    `json:"api_equivalent_cost_partial,omitempty"`
	CostSource               string  `json:"cost_source,omitempty"`
}

type ProviderQueueStatus struct {
	Provider string     `json:"provider"`
	Position int        `json:"position"`
	Ahead    int        `json:"ahead"`
	Active   int        `json:"active"`
	Limit    int        `json:"limit"`
	WaitMS   int64      `json:"wait_ms"`
	ETAAt    *time.Time `json:"eta_at,omitempty"`
}

type CheckResult struct {
	Name            string   `json:"name"`
	Profile         string   `json:"profile,omitempty"`
	Status          string   `json:"status"`
	Duration        Duration `json:"duration_ms"`
	ExitCode        int      `json:"exit_code,omitempty"`
	Error           string   `json:"error,omitempty"`
	Isolation       string   `json:"isolation,omitempty"`
	ReusedFromRunID string   `json:"reused_from_run_id,omitempty"`
}

type SecurityMetadata struct {
	ReviewerIsolation   string   `json:"reviewer_isolation"`
	RepositoryPolicy    string   `json:"repository_policy"`
	ControlFilesChanged []string `json:"control_files_changed,omitempty"`
	CheckExecution      string   `json:"check_execution"`
}

type EscalationMetadata struct {
	Triggered      bool     `json:"triggered"`
	Causes         []string `json:"causes,omitempty"`
	SensitivePaths []string `json:"sensitive_paths,omitempty"`
}

type Decision struct {
	SchemaVersion        string                `json:"schema_version"`
	RunID                string                `json:"run_id"`
	State                string                `json:"state"`
	OutcomeQualifier     string                `json:"outcome_qualifier,omitempty"`
	StrictPolicy         bool                  `json:"strict_policy"`
	ValidationStatus     string                `json:"validation_status"`
	Reason               string                `json:"reason"`
	BaseSHA              string                `json:"base_sha"`
	HeadSHA              string                `json:"head_sha"`
	DiffHash             string                `json:"diff_hash"`
	Reviewers            map[string]string     `json:"reviewers"`
	ReviewerErrors       map[string]string     `json:"reviewer_errors,omitempty"`
	OpenFindings         map[string]int        `json:"open_findings"`
	Findings             []ConsolidatedFinding `json:"findings,omitempty"`
	CarryForwardFindings []ConsolidatedFinding `json:"carry_forward_findings"`
	RejectedFindings     []ConsolidatedFinding `json:"rejected_findings,omitempty"`
	CrossExaminations    []CrossExamination    `json:"cross_examinations,omitempty"`
	ResidualRisks        []string              `json:"residual_risks,omitempty"`
	Disagreements        []string              `json:"disagreements,omitempty"`
	Checks               map[string]string     `json:"checks,omitempty"`
	Usage                Usage                 `json:"usage"`
	IncrementalUsage     Usage                 `json:"incremental_usage"`
	CumulativeUsage      Usage                 `json:"cumulative_usage"`
	CreatedAt            time.Time             `json:"created_at"`
	RecordPath           string                `json:"record_path,omitempty"`
}

type Manifest struct {
	SchemaVersion         string                `json:"schema_version"`
	RunID                 string                `json:"run_id"`
	Repository            string                `json:"repository"`
	RepositoryIdentity    string                `json:"repository_identity"`
	StartedAt             time.Time             `json:"started_at"`
	FinishedAt            time.Time             `json:"finished_at,omitempty"`
	WallElapsed           Duration              `json:"wall_elapsed_ms"`
	ActiveExecution       Duration              `json:"active_execution_ms"`
	ActiveTimingBasis     string                `json:"active_timing_basis,omitempty"`
	ParentRunID           string                `json:"parent_run_id,omitempty"`
	AutoFixLoopID         string                `json:"auto_fix_loop_id,omitempty"`
	AutoFixIteration      int                   `json:"auto_fix_iteration,omitempty"`
	ReviewScope           string                `json:"review_scope,omitempty"`
	ApprovalBaselineRunID string                `json:"approval_baseline_run_id,omitempty"`
	ApprovalBaselineHash  string                `json:"approval_baseline_diff_hash,omitempty"`
	Target                Target                `json:"target"`
	FullTarget            *Target               `json:"full_target,omitempty"`
	ReviewPolicy          *AutoFixReviewPolicy  `json:"review_policy,omitempty"`
	Reviewers             []ReviewerResult      `json:"reviewers,omitempty"`
	SecurityReviews       []ReviewerResult      `json:"security_reviews,omitempty"`
	CrossExaminations     []ReviewerResult      `json:"cross_examinations,omitempty"`
	CarriedFindings       []ConsolidatedFinding `json:"carried_findings,omitempty"`
	Checks                []CheckResult         `json:"checks,omitempty"`
	PromptHash            string                `json:"prompt_hash"`
	SecurityPromptHash    string                `json:"security_review_prompt_hash,omitempty"`
	CrossExamPromptHash   string                `json:"cross_examination_prompt_hash,omitempty"`
	PolicyHash            string                `json:"policy_hash"`
	SchemaHash            string                `json:"schema_hash"`
	CoraVersion           string                `json:"cora_version"`
	CoraSourceSHA         string                `json:"cora_source_sha"`
	CoraBuildTime         string                `json:"cora_build_time,omitempty"`
	Security              SecurityMetadata      `json:"security"`
	Escalation            EscalationMetadata    `json:"escalation"`
	StrictPolicy          bool                  `json:"strict_policy"`
	Usage                 Usage                 `json:"usage"`
	IncrementalUsage      Usage                 `json:"incremental_usage"`
	CumulativeUsage       Usage                 `json:"cumulative_usage"`
}

type AutoFixAttempt struct {
	Agent             string     `json:"agent"`
	Status            string     `json:"status"`
	Model             string     `json:"model"`
	ModelSource       string     `json:"model_source,omitempty"`
	Effort            string     `json:"effort"`
	Tool              string     `json:"tool"`
	ToolVersion       string     `json:"tool_version,omitempty"`
	Auth              string     `json:"auth,omitempty"`
	Duration          Duration   `json:"duration_ms"`
	QueueDuration     Duration   `json:"queue_duration_ms"`
	ExecutionDuration Duration   `json:"execution_duration_ms"`
	ExitCode          int        `json:"exit_code,omitempty"`
	PromptHash        string     `json:"prompt_hash"`
	PolicyHash        string     `json:"policy_hash"`
	BeforeDiffHash    string     `json:"before_diff_hash"`
	AfterDiffHash     string     `json:"after_diff_hash,omitempty"`
	ChangedPaths      []string   `json:"changed_paths,omitempty"`
	Usage             Usage      `json:"usage"`
	FailureKind       string     `json:"failure_kind,omitempty"`
	Retryable         bool       `json:"retryable,omitempty"`
	RetryAt           *time.Time `json:"retry_at,omitempty"`
	Error             string     `json:"error,omitempty"`
}

type AutoFixIteration struct {
	Number                int              `json:"number"`
	ReviewRunID           string           `json:"review_run_id"`
	ReviewAttemptRunIDs   []string         `json:"review_attempt_run_ids,omitempty"`
	ReviewRecordPath      string           `json:"review_record_path"`
	ReviewState           string           `json:"review_state"`
	ReviewScope           string           `json:"review_scope,omitempty"`
	ReviewDiffHash        string           `json:"review_diff_hash"`
	FullDiffHash          string           `json:"full_diff_hash,omitempty"`
	ApprovalBaselineRunID string           `json:"approval_baseline_run_id,omitempty"`
	ApprovalBaselineHash  string           `json:"approval_baseline_diff_hash,omitempty"`
	QualifyingFindingIDs  []string         `json:"qualifying_finding_ids,omitempty"`
	QualifyingFingerprint string           `json:"qualifying_fingerprint,omitempty"`
	ReviewUsage           Usage            `json:"review_usage"`
	Fix                   *AutoFixAttempt  `json:"fix,omitempty"`
	FixAttempts           []AutoFixAttempt `json:"fix_attempts,omitempty"`
}

// AutoFixReviewContext describes how an auto-fix child review relates to a
// previously approved exact diff. ReviewScope is advisory only until the
// review runner binds it into its manifest and prompt; FullTarget always
// identifies the complete current diff that final approval must attest.
type AutoFixReviewContext struct {
	ReviewScope           string                `json:"review_scope"`
	ApprovalBaselineRunID string                `json:"approval_baseline_run_id,omitempty"`
	ApprovalBaselineHash  string                `json:"approval_baseline_diff_hash,omitempty"`
	TrustedBaseSHA        string                `json:"trusted_base_sha"`
	FullTarget            Target                `json:"full_target"`
	BaselineFindings      []ConsolidatedFinding `json:"baseline_findings,omitempty"`
}

// AutoFixReviewPolicy is the immutable effective review configuration used by
// every child review in a resumable auto-fix loop. Checks are stored after
// profile expansion so a resume cannot silently weaken validation by loading
// different defaults or selecting a different profile.
type AutoFixReviewPolicy struct {
	ReviewerTimeout              Duration                      `json:"reviewer_timeout_ms"`
	OverallTimeout               Duration                      `json:"overall_timeout_ms"`
	QueueTimeout                 Duration                      `json:"queue_timeout_ms"`
	StrictPolicy                 bool                          `json:"strict_policy"`
	CrossExamineBlockingFindings bool                          `json:"cross_examine_blocking_findings"`
	RequireCleanTree             bool                          `json:"require_clean_tree"`
	AllowAPIBilling              bool                          `json:"allow_api_billing"`
	AllowUnsafeChecks            bool                          `json:"allow_unsafe_host_checks"`
	MinimumApprovals             int                           `json:"minimum_approvals"`
	BlockingSeverities           []string                      `json:"blocking_severities"`
	PromptFile                   string                        `json:"prompt_file,omitempty"`
	Codex                        AutoFixReviewerPolicy         `json:"codex"`
	Claude                       AutoFixReviewerPolicy         `json:"claude"`
	Escalation                   AutoFixEscalationPolicy       `json:"escalation"`
	CrossExamination             AutoFixCrossExaminationPolicy `json:"cross_examination"`
	Checks                       []AutoFixCheckPolicy          `json:"checks"`
}

type AutoFixReviewerPolicy struct {
	Enabled           bool    `json:"enabled"`
	Command           string  `json:"command"`
	Model             string  `json:"model"`
	Effort            string  `json:"effort"`
	MaxTurns          int     `json:"max_turns"`
	FinalizationTurns int     `json:"finalization_turns"`
	MaxBudgetUSD      float64 `json:"max_budget_usd"`
	MaxConcurrency    int     `json:"max_concurrency"`
}

type AutoFixEscalationPolicy struct {
	Enabled                 bool     `json:"enabled"`
	Model                   string   `json:"model"`
	Effort                  string   `json:"effort"`
	MaxTurns                *int     `json:"max_turns,omitempty"`
	MaxBudgetUSD            *float64 `json:"max_budget_usd,omitempty"`
	SecurityPathMarkers     []string `json:"security_path_markers"`
	ForceSecuritySensitive  bool     `json:"force_security_sensitive"`
	AdjudicateDisagreements bool     `json:"adjudicate_disagreements"`
}

type AutoFixCrossExaminationPolicy struct {
	Timeout      Duration `json:"timeout_ms"`
	MaxTurns     int      `json:"max_turns"`
	MaxBudgetUSD float64  `json:"max_budget_usd"`
}

type AutoFixCheckPolicy struct {
	Name         string   `json:"name"`
	Command      []string `json:"command"`
	Timeout      Duration `json:"timeout_ms"`
	EnvAllowlist []string `json:"env_allowlist,omitempty"`
	Profile      string   `json:"profile,omitempty"`
}

type AutoFixLoop struct {
	SchemaVersion       string               `json:"schema_version"`
	LoopID              string               `json:"loop_id"`
	State               string               `json:"state"`
	Reason              string               `json:"reason"`
	Repository          string               `json:"repository"`
	RepositoryIdentity  string               `json:"repository_identity"`
	BaseRef             string               `json:"base_ref"`
	BaseSHA             string               `json:"base_sha"`
	InitialHeadSHA      string               `json:"initial_head_sha"`
	InitialDiffHash     string               `json:"initial_diff_hash"`
	FinalDiffHash       string               `json:"final_diff_hash,omitempty"`
	BaselineRunID       string               `json:"approved_baseline_run_id,omitempty"`
	BaselineDiffHash    string               `json:"approved_baseline_diff_hash,omitempty"`
	ReviewPolicy        *AutoFixReviewPolicy `json:"review_policy,omitempty"`
	Threshold           string               `json:"until"`
	Agent               string               `json:"agent"`
	AgentCommand        string               `json:"agent_command"`
	AgentModel          string               `json:"agent_model"`
	AgentEffort         string               `json:"agent_effort"`
	AgentMaxConcurrency int                  `json:"agent_max_concurrency"`
	AgentTimeout        Duration             `json:"agent_timeout_ms"`
	MaxIterations       int                  `json:"max_iterations"`
	MaxDuration         Duration             `json:"max_duration_ms"`
	MaxTurns            int                  `json:"max_turns"`
	MaxCostUSD          float64              `json:"max_cost_usd"`
	StartedAt           time.Time            `json:"started_at"`
	FinishedAt          *time.Time           `json:"finished_at,omitempty"`
	PausedAt            *time.Time           `json:"paused_at,omitempty"`
	RetryAt             *time.Time           `json:"retry_at,omitempty"`
	ResumeReviewRunID   string               `json:"resume_review_run_id,omitempty"`
	ResumeReviewers     []string             `json:"resume_reviewers,omitempty"`
	ResumeIteration     int                  `json:"resume_iteration,omitempty"`
	ResumePhase         string               `json:"resume_phase,omitempty"`
	PausedDuration      Duration             `json:"paused_duration_ms"`
	ResumeCount         int                  `json:"resume_count,omitempty"`
	Elapsed             Duration             `json:"elapsed_ms"`
	Iterations          []AutoFixIteration   `json:"iterations"`
	Usage               Usage                `json:"usage"`
	CoraVersion         string               `json:"cora_version"`
	CoraSourceSHA       string               `json:"cora_source_sha"`
	CoraBuildTime       string               `json:"cora_build_time,omitempty"`
	FinalDecision       *Decision            `json:"final_decision,omitempty"`
	RecordPath          string               `json:"record_path"`
}

type Heartbeat struct {
	RunID             string                         `json:"run_id"`
	State             string                         `json:"state"`
	Phase             string                         `json:"phase"`
	StartedAt         time.Time                      `json:"started_at"`
	UpdatedAt         time.Time                      `json:"updated_at"`
	WallElapsed       Duration                       `json:"wall_elapsed_ms"`
	ActiveExecution   Duration                       `json:"active_execution_ms"`
	ActiveTimingBasis string                         `json:"active_timing_basis,omitempty"`
	PID               int                            `json:"pid"`
	Reviewers         map[string]string              `json:"reviewers,omitempty"`
	ReviewerStartedAt map[string]time.Time           `json:"reviewer_started_at,omitempty"`
	Checks            map[string]string              `json:"checks,omitempty"`
	Queues            map[string]ProviderQueueStatus `json:"queues,omitempty"`
}

type RunSummary struct {
	RunID              string                         `json:"run_id"`
	State              string                         `json:"state"`
	Phase              string                         `json:"phase,omitempty"`
	StartedAt          time.Time                      `json:"started_at"`
	FinishedAt         time.Time                      `json:"finished_at,omitempty"`
	ElapsedMS          int64                          `json:"elapsed_ms"`
	ActiveExecutionMS  int64                          `json:"active_execution_ms"`
	ActiveTimingBasis  string                         `json:"active_timing_basis,omitempty"`
	HeadSHA            string                         `json:"head_sha"`
	ParentRunID        string                         `json:"parent_run_id,omitempty"`
	AutoFixLoopID      string                         `json:"auto_fix_loop_id,omitempty"`
	AutoFixIteration   int                            `json:"auto_fix_iteration,omitempty"`
	RepositoryIdentity string                         `json:"repository_identity,omitempty"`
	Reviewers          map[string]string              `json:"reviewers,omitempty"`
	ReviewerElapsedMS  map[string]int64               `json:"reviewer_elapsed_ms,omitempty"`
	Checks             map[string]string              `json:"checks,omitempty"`
	Queues             map[string]ProviderQueueStatus `json:"queues,omitempty"`
	RecordPath         string                         `json:"record_path"`
}
