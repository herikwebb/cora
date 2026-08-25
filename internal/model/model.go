package model

import "time"

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
	Duration        time.Duration `json:"duration"`
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
	Usage           Usage         `json:"usage"`
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
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration"`
	ExitCode int           `json:"exit_code,omitempty"`
	Error    string        `json:"error,omitempty"`
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
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id"`
	State         string            `json:"state"`
	Reason        string            `json:"reason"`
	BaseSHA       string            `json:"base_sha"`
	HeadSHA       string            `json:"head_sha"`
	DiffHash      string            `json:"diff_hash"`
	Reviewers     map[string]string `json:"reviewers"`
	OpenFindings  map[string]int    `json:"open_findings"`
	Checks        map[string]string `json:"checks,omitempty"`
	Usage         Usage             `json:"usage"`
	CreatedAt     time.Time         `json:"created_at"`
	RecordPath    string            `json:"record_path,omitempty"`
}

type Manifest struct {
	SchemaVersion string             `json:"schema_version"`
	RunID         string             `json:"run_id"`
	Repository    string             `json:"repository"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at,omitempty"`
	Target        Target             `json:"target"`
	Reviewers     []ReviewerResult   `json:"reviewers,omitempty"`
	Checks        []CheckResult      `json:"checks,omitempty"`
	PromptHash    string             `json:"prompt_hash"`
	PolicyHash    string             `json:"policy_hash"`
	SchemaHash    string             `json:"schema_hash"`
	CoraVersion   string             `json:"cora_version"`
	Security      SecurityMetadata   `json:"security"`
	Escalation    EscalationMetadata `json:"escalation"`
	Usage         Usage              `json:"usage"`
}
