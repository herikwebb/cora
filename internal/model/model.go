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
	ID           string  `json:"id"`
	Severity     string  `json:"severity"`
	Confidence   float64 `json:"confidence"`
	File         string  `json:"file"`
	Line         int     `json:"line"`
	Claim        string  `json:"claim"`
	Evidence     string  `json:"evidence"`
	SuggestedFix string  `json:"suggested_fix"`
}

type ConsolidatedFinding struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"`
	Confidence     float64  `json:"confidence"`
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Claim          string   `json:"claim"`
	Evidence       []string `json:"evidence"`
	SuggestedFixes []string `json:"suggested_fixes"`
	Reviewers      []string `json:"reviewers"`
	SourceIDs      []string `json:"source_ids"`
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
	Reviewer        string        `json:"reviewer"`
	Status          string        `json:"status"`
	Duration        Duration      `json:"duration_ms"`
	Report          *ReviewReport `json:"report,omitempty"`
	Error           string        `json:"error,omitempty"`
	ExitCode        int           `json:"exit_code,omitempty"`
	Auth            string        `json:"auth,omitempty"`
	Tool            string        `json:"tool,omitempty"`
	ToolVersion     string        `json:"tool_version,omitempty"`
	Model           string        `json:"model,omitempty"`
	ModelSource     string        `json:"model_source,omitempty"`
	Effort          string        `json:"effort,omitempty"`
	EscalationCause string        `json:"escalation_cause,omitempty"`
	Attempt         int           `json:"attempt"`
	ReusedFromRunID string        `json:"reused_from_run_id,omitempty"`
	FailureKind     string        `json:"failure_kind,omitempty"`
	Retryable       bool          `json:"retryable,omitempty"`
	RetryAt         *time.Time    `json:"retry_at,omitempty"`
	Usage           Usage         `json:"usage"`
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
	SchemaVersion string                `json:"schema_version"`
	RunID         string                `json:"run_id"`
	State         string                `json:"state"`
	Reason        string                `json:"reason"`
	BaseSHA       string                `json:"base_sha"`
	HeadSHA       string                `json:"head_sha"`
	DiffHash      string                `json:"diff_hash"`
	Reviewers     map[string]string     `json:"reviewers"`
	OpenFindings  map[string]int        `json:"open_findings"`
	Findings      []ConsolidatedFinding `json:"findings,omitempty"`
	ResidualRisks []string              `json:"residual_risks,omitempty"`
	Disagreements []string              `json:"disagreements,omitempty"`
	Checks        map[string]string     `json:"checks,omitempty"`
	Usage         Usage                 `json:"usage"`
	CreatedAt     time.Time             `json:"created_at"`
	RecordPath    string                `json:"record_path,omitempty"`
}

type Manifest struct {
	SchemaVersion      string             `json:"schema_version"`
	RunID              string             `json:"run_id"`
	Repository         string             `json:"repository"`
	RepositoryIdentity string             `json:"repository_identity"`
	StartedAt          time.Time          `json:"started_at"`
	FinishedAt         time.Time          `json:"finished_at,omitempty"`
	ParentRunID        string             `json:"parent_run_id,omitempty"`
	Target             Target             `json:"target"`
	Reviewers          []ReviewerResult   `json:"reviewers,omitempty"`
	Checks             []CheckResult      `json:"checks,omitempty"`
	PromptHash         string             `json:"prompt_hash"`
	PolicyHash         string             `json:"policy_hash"`
	SchemaHash         string             `json:"schema_hash"`
	CoraVersion        string             `json:"cora_version"`
	CoraSourceSHA      string             `json:"cora_source_sha"`
	CoraBuildTime      string             `json:"cora_build_time,omitempty"`
	Security           SecurityMetadata   `json:"security"`
	Escalation         EscalationMetadata `json:"escalation"`
	Usage              Usage              `json:"usage"`
}

type Heartbeat struct {
	RunID     string            `json:"run_id"`
	State     string            `json:"state"`
	Phase     string            `json:"phase"`
	StartedAt time.Time         `json:"started_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	PID       int               `json:"pid"`
	Reviewers map[string]string `json:"reviewers,omitempty"`
	Checks    map[string]string `json:"checks,omitempty"`
}

type RunSummary struct {
	RunID              string            `json:"run_id"`
	State              string            `json:"state"`
	Phase              string            `json:"phase,omitempty"`
	StartedAt          time.Time         `json:"started_at"`
	FinishedAt         time.Time         `json:"finished_at,omitempty"`
	ElapsedMS          int64             `json:"elapsed_ms"`
	HeadSHA            string            `json:"head_sha"`
	ParentRunID        string            `json:"parent_run_id,omitempty"`
	RepositoryIdentity string            `json:"repository_identity,omitempty"`
	Reviewers          map[string]string `json:"reviewers,omitempty"`
	Checks             map[string]string `json:"checks,omitempty"`
	RecordPath         string            `json:"record_path"`
}
