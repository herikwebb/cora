package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
	processx "github.com/herikwebb/cora/internal/process"
)

type Request struct {
	RepoRoot        string
	WorkDir         string
	RuntimeDir      string
	RecoveryDir     string
	Target          model.Target
	RunDir          string
	SchemaPath      string
	Schema          []byte
	Prompt          string
	Policy          string
	Timeout         time.Duration
	AllowAPIBilling bool
	Attempt         int
	ChangedPaths    []string
}

type FixRequest struct {
	RepoRoot        string
	RecordDir       string
	Prompt          string
	Policy          string
	Timeout         time.Duration
	AllowAPIBilling bool
}

type Adapter interface {
	Name() string
	Provider() string
	Review(context.Context, Request) model.ReviewerResult
}

type AdapterDescriptor struct {
	Model           string
	Effort          string
	EscalationCause string
}

// DescribeAdapter returns the configured review metadata available before an
// adapter executes. Unknown adapter implementations have an empty descriptor.
func DescribeAdapter(adapter Adapter) AdapterDescriptor {
	switch value := adapter.(type) {
	case Codex:
		return AdapterDescriptor{Model: value.Config.Model, Effort: value.Config.Effort, EscalationCause: value.EscalationCause}
	case *Codex:
		if value != nil {
			return AdapterDescriptor{Model: value.Config.Model, Effort: value.Config.Effort, EscalationCause: value.EscalationCause}
		}
	case Claude:
		return AdapterDescriptor{Model: value.Config.Model, Effort: value.Config.Effort, EscalationCause: value.EscalationCause}
	case *Claude:
		if value != nil {
			return AdapterDescriptor{Model: value.Config.Model, Effort: value.Config.Effort, EscalationCause: value.EscalationCause}
		}
	}
	return AdapterDescriptor{}
}

func Enabled(cfg config.Config) []Adapter {
	return EnabledWithClaudeEscalation(cfg, "")
}

func EnabledWithClaudeEscalation(cfg config.Config, cause string) []Adapter {
	adapters := make([]Adapter, 0, 2)
	if cfg.Reviewers.Codex.Enabled {
		adapters = append(adapters, Codex{Config: cfg.Reviewers.Codex})
	}
	if cfg.Reviewers.Claude.Enabled {
		adapters = append(adapters, Claude{Config: cfg.Reviewers.Claude, EscalationCause: cause})
	}
	return adapters
}

type Codex struct {
	Config          config.Reviewer
	ReviewerName    string
	EscalationCause string
}

func (c Codex) Name() string {
	if c.ReviewerName != "" {
		return c.ReviewerName
	}
	return "codex"
}
func (Codex) Provider() string { return "codex" }

func (c Codex) Review(parent context.Context, request Request) model.ReviewerResult {
	started := time.Now()
	result := model.ReviewerResult{
		Reviewer: c.Name(), Status: "incomplete", Tool: c.Config.Command, Attempt: normalizedAttempt(request.Attempt),
		Model: c.Config.Model, ModelSource: "configured", Effort: c.Config.Effort,
		EscalationCause: c.EscalationCause,
	}
	env := processx.ReviewerWorkspaceEnvironment(request.AllowAPIBilling, request.RuntimeDir)

	path, err := lookPathWithFallback(c.Config.Command, codexFallbackPaths())
	if err != nil {
		result.Error = fmt.Sprintf("find Codex CLI: %v", err)
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	result.Tool = path
	versionCtx, cancelVersion := context.WithTimeout(parent, 10*time.Second)
	version, _, _ := processx.Capture(versionCtx, path, request.WorkDir, env, "--version")
	cancelVersion()
	result.ToolVersion = strings.TrimSpace(string(version))

	authCtx, cancelAuth := context.WithTimeout(parent, 15*time.Second)
	authOut, authErrOut, authResult := processx.Capture(authCtx, path, request.WorkDir, env, "login", "status")
	cancelAuth()
	if authResult.Err != nil {
		result.Error = "Codex authentication check failed: " + firstNonEmpty(string(authErrOut), authResult.Err.Error())
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	if codexUsesChatGPT(authOut, authErrOut) {
		result.Auth = "chatgpt"
	} else if request.AllowAPIBilling {
		result.Auth = "api-or-other"
	} else {
		result.Error = "Codex is not authenticated with ChatGPT; refusing possible API billing"
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}

	rawPath := filepath.Join(request.RunDir, "codex.raw.json")
	if err := preparePrivateFile(rawPath); err != nil {
		result.Error = "prepare Codex output: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	effectivePrompt, checkpointPath, err := prepareReviewerPrompt(request, result.Reviewer, request.Prompt)
	if err != nil {
		result.Error = "prepare Codex recovery checkpoint: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	if err := persistReviewerPrompt(request.RunDir, result.Reviewer, effectivePrompt); err != nil {
		result.Error = "persist effective Codex prompt: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	args := codexReviewArgs(c.Config, request, rawPath)
	stderrPath := filepath.Join(request.RunDir, "codex.stderr.log")

	reviewCtx, cancelReview := context.WithTimeout(parent, request.Timeout)
	eventsPath := filepath.Join(request.RunDir, "codex.events.jsonl")
	processResult := processx.Run(reviewCtx, processx.Spec{
		Command:    path,
		Args:       args,
		Dir:        request.WorkDir,
		Stdin:      []byte(effectivePrompt),
		Env:        env,
		StdoutPath: eventsPath,
		StderrPath: stderrPath,
	})
	cancelReview()
	result.Duration = model.NewDuration(time.Since(started))
	result.ExitCode = processResult.ExitCode
	if telemetry, err := readCodexTelemetry(eventsPath, result.Model); err == nil {
		applyTelemetry(&result, telemetry)
	}
	if processResult.Err != nil {
		result.Error = "Codex review failed: " + codexFailure(eventsPath, stderrPath, processResult.Err)
		classifyFailure(&result, time.Now())
		if errors.Is(processResult.Err, context.DeadlineExceeded) {
			result.FailureKind = "timeout"
			partial, found := readCodexPartialReport(checkpointPath, rawPath, eventsPath)
			if found {
				attachPartialReviewerReport(&result, request, &partial, "Codex review timed out before producing a complete report.")
			} else {
				attachPartialReviewerReport(&result, request, nil, "Codex review timed out before producing a complete report.")
			}
		}
		return result
	}
	report, err := readReport(rawPath)
	if err != nil {
		result.Error = "parse Codex report: " + err.Error()
		return result
	}
	attachTarget(&report, result.Reviewer, request.Target)
	if err := validateReport(report); err != nil {
		result.Error = "validate Codex report: " + err.Error()
		return result
	}
	result.Status = "completed"
	result.Report = &report
	return result
}

func codexReviewArgs(cfg config.Reviewer, request Request, rawPath string) []string {
	// The built-in `codex exec review` target flags cannot be combined with a
	// custom prompt. CORA uses generic exec so its audited prompt can describe
	// the exact target and enforce the shared reviewer policy and schema.
	args := []string{
		"exec",
		"--sandbox", "workspace-write",
		"--cd", request.WorkDir,
		"--skip-git-repo-check",
		"--ignore-rules",
		"--ephemeral",
		"--ignore-user-config",
		"--config", "developer_instructions=" + strconv.Quote(request.Policy),
	}
	if request.RuntimeDir != "" {
		args = append(args, "--add-dir", request.RuntimeDir)
	}
	if request.RecoveryDir != "" {
		args = append(args, "--add-dir", request.RecoveryDir)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--config", "model_reasoning_effort="+strconv.Quote(cfg.Effort))
	}
	args = append(args,
		"--output-schema", request.SchemaPath,
		"--json",
		"--output-last-message", rawPath,
		"-",
	)
	return args
}

func prepareReviewerPrompt(request Request, reviewer, prompt string) (string, string, error) {
	if strings.TrimSpace(request.RecoveryDir) == "" {
		return prompt, "", nil
	}
	checkpoint, err := os.CreateTemp(request.RecoveryDir, fileStem(reviewer)+"-*.partial-checkpoint.json")
	if err != nil {
		return "", "", err
	}
	checkpointPath := checkpoint.Name()
	if err := checkpoint.Chmod(0o600); err != nil {
		_ = checkpoint.Close()
		_ = os.Remove(checkpointPath)
		return "", "", err
	}
	if err := checkpoint.Close(); err != nil {
		_ = os.Remove(checkpointPath)
		return "", "", err
	}
	contract := fmt.Sprintf(`

CORA interruption-recovery checkpoint contract:
- Only after you confirm at least one finding, overwrite %s with a best-effort report using the exact supplied JSON schema.
- A checkpoint must use verdict "abstain" and context_complete false. Include every confirmed finding so far, plus the reviewed_paths, omitted_paths, and residual_risks known at that point.
- Update the checkpoint only when the set or substance of confirmed findings changes. Do not write checkpoints for suspicions or when there are no confirmed findings.
- The checkpoint is recovery-only. Continue the review and return a fresh, complete structured report normally.
`, strconv.Quote(checkpointPath))
	return prompt + contract, checkpointPath, nil
}

func persistReviewerPrompt(runDir, reviewer, prompt string) error {
	if strings.TrimSpace(runDir) == "" {
		return nil
	}
	path := filepath.Join(runDir, fileStem(reviewer)+".effective-prompt.md")
	if err := preparePrivateFile(path); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(prompt), 0o600)
}

func RunCodexFix(parent context.Context, cfg config.AutoFix, request FixRequest) model.AutoFixAttempt {
	started := time.Now()
	result := model.AutoFixAttempt{
		Agent: "codex", Status: "incomplete", Tool: cfg.Command,
		Model: cfg.Model, ModelSource: "configured", Effort: cfg.Effort,
	}
	env := processx.ReviewerEnvironment(request.AllowAPIBilling)
	path, err := lookPathWithFallback(cfg.Command, codexFallbackPaths())
	if err != nil {
		result.Error = fmt.Sprintf("find Codex CLI: %v", err)
		result.ExecutionDuration = model.NewDuration(time.Since(started))
		return result
	}
	result.Tool = path
	versionCtx, cancelVersion := context.WithTimeout(parent, 10*time.Second)
	version, _, _ := processx.Capture(versionCtx, path, request.RepoRoot, env, "--version")
	cancelVersion()
	result.ToolVersion = strings.TrimSpace(string(version))
	authCtx, cancelAuth := context.WithTimeout(parent, 15*time.Second)
	authOut, authErrOut, authResult := processx.Capture(authCtx, path, request.RepoRoot, env, "login", "status")
	cancelAuth()
	if authResult.Err != nil {
		result.Error = "Codex authentication check failed: " + firstNonEmpty(string(authErrOut), authResult.Err.Error())
		result.ExecutionDuration = model.NewDuration(time.Since(started))
		return result
	}
	if codexUsesChatGPT(authOut, authErrOut) {
		result.Auth = "chatgpt"
	} else if request.AllowAPIBilling {
		result.Auth = "api-or-other"
	} else {
		result.Error = "Codex is not authenticated with ChatGPT; refusing possible API billing"
		result.ExecutionDuration = model.NewDuration(time.Since(started))
		return result
	}

	lastMessagePath := filepath.Join(request.RecordDir, "agent.last.txt")
	if err := preparePrivateFile(lastMessagePath); err != nil {
		result.Error = "prepare coding-agent output: " + err.Error()
		result.ExecutionDuration = model.NewDuration(time.Since(started))
		return result
	}
	args := codexFixArgs(cfg, request, lastMessagePath)
	eventsPath := filepath.Join(request.RecordDir, "agent.events.jsonl")
	stderrPath := filepath.Join(request.RecordDir, "agent.stderr.log")
	fixCtx, cancelFix := context.WithTimeout(parent, request.Timeout)
	processResult := processx.Run(fixCtx, processx.Spec{
		Command: path, Args: args, Dir: request.RepoRoot, Stdin: []byte(request.Prompt), Env: env,
		StdoutPath: eventsPath, StderrPath: stderrPath,
	})
	cancelFix()
	result.ExecutionDuration = model.NewDuration(time.Since(started))
	result.ExitCode = processResult.ExitCode
	if telemetry, telemetryErr := readCodexTelemetry(eventsPath, result.Model); telemetryErr == nil {
		applyFixTelemetry(&result, telemetry)
	}
	if processResult.Err != nil {
		result.Error = "Codex coding agent failed: " + codexFailure(eventsPath, stderrPath, processResult.Err)
		return result
	}
	result.Status = "completed"
	return result
}

func codexFixArgs(cfg config.AutoFix, request FixRequest, lastMessagePath string) []string {
	args := []string{
		"exec", "--sandbox", "workspace-write", "--cd", request.RepoRoot,
		"--skip-git-repo-check", "--ignore-rules", "--ephemeral", "--ignore-user-config",
		"--config", "developer_instructions=" + strconv.Quote(request.Policy),
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--config", "model_reasoning_effort="+strconv.Quote(cfg.Effort))
	}
	args = append(args, "--json", "--output-last-message", lastMessagePath, "-")
	return args
}

func applyFixTelemetry(result *model.AutoFixAttempt, telemetry reviewerTelemetry) {
	if telemetry.Model != "" {
		result.Model = telemetry.Model
		result.ModelSource = telemetry.ModelSource
	}
	result.Usage = telemetry.Usage
}

type Claude struct {
	Config          config.Reviewer
	ReviewerName    string
	EscalationCause string
}

func claudeReviewerSandboxSettings(runtimeDir, recoveryDir string) string {
	filesystem := map[string]any{}
	var writable []string
	if runtimeDir != "" {
		writable = append(writable, runtimeDir)
	}
	if recoveryDir != "" {
		writable = append(writable, recoveryDir)
	}
	if len(writable) > 0 {
		filesystem["allowWrite"] = writable
	}
	settings := map[string]any{
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true,
			"autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
			"filesystem": filesystem,
			"network": map[string]any{
				"allowedDomains": []string{}, "deniedDomains": []string{"*"}, "strictAllowlist": true,
			},
		},
	}
	encoded, _ := json.Marshal(settings)
	return string(encoded)
}

func (c Claude) Name() string {
	if c.ReviewerName != "" {
		return c.ReviewerName
	}
	return "claude"
}

func (Claude) Provider() string { return "claude" }

func (c Claude) Review(parent context.Context, request Request) model.ReviewerResult {
	started := time.Now()
	result := model.ReviewerResult{
		Reviewer: c.Name(), Status: "incomplete", Tool: c.Config.Command, Attempt: normalizedAttempt(request.Attempt),
		Model: c.Config.Model, ModelSource: "configured", Effort: c.Config.Effort,
		EscalationCause: c.EscalationCause,
	}
	env := processx.ReviewerWorkspaceEnvironment(request.AllowAPIBilling, request.RuntimeDir)

	path, err := exec.LookPath(c.Config.Command)
	if err != nil {
		result.Error = fmt.Sprintf("find Claude CLI: %v", err)
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	result.Tool = path
	versionCtx, cancelVersion := context.WithTimeout(parent, 10*time.Second)
	version, _, _ := processx.Capture(versionCtx, path, request.WorkDir, env, "--version")
	cancelVersion()
	result.ToolVersion = strings.TrimSpace(string(version))

	authCtx, cancelAuth := context.WithTimeout(parent, 15*time.Second)
	authOut, authErrOut, authResult := processx.Capture(authCtx, path, request.WorkDir, env, "auth", "status")
	cancelAuth()
	if authResult.Err != nil {
		result.Error = "Claude authentication check failed: " + firstNonEmpty(string(authErrOut), authResult.Err.Error())
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	var auth struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIProvider      string `json:"apiProvider"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(authOut, &auth); err != nil {
		result.Error = "parse Claude authentication status: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	if auth.LoggedIn && auth.AuthMethod == "claude.ai" && strings.EqualFold(auth.APIProvider, "firstParty") && auth.SubscriptionType != "" {
		result.Auth = "claude.ai:" + auth.SubscriptionType
	} else if request.AllowAPIBilling && auth.LoggedIn {
		result.Auth = "api-or-other"
	} else {
		result.Error = "Claude is not authenticated with a Claude.ai subscription; refusing possible API billing"
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}

	compactSchema, err := schemaForClaude(request.Schema)
	if err != nil {
		result.Error = "prepare Claude output schema: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	effectivePrompt, checkpointPath, err := prepareReviewerPrompt(request, result.Reviewer, request.Prompt)
	if err != nil {
		result.Error = "prepare Claude recovery checkpoint: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	effectivePrompt = claudePrompt(effectivePrompt, c.Config)
	if err := persistReviewerPrompt(request.RunDir, result.Reviewer, effectivePrompt); err != nil {
		result.Error = "persist effective Claude prompt: " + err.Error()
		result.Duration = model.NewDuration(time.Since(started))
		return result
	}
	inspectionTurns := c.Config.MaxTurns - c.Config.FinalizationTurns
	args := claudeReviewArgs(c.Config, request, compactSchema, effectivePrompt, inspectionTurns, "Read,Glob,Grep,Bash")

	rawPath := filepath.Join(request.RunDir, fileStem(c.Name())+".raw.json")
	stderrPath := filepath.Join(request.RunDir, fileStem(c.Name())+".stderr.log")
	reviewCtx, cancelReview := context.WithTimeout(parent, request.Timeout)
	defer cancelReview()
	processResult := processx.Run(reviewCtx, processx.Spec{
		Command:    path,
		Args:       args,
		Dir:        request.WorkDir,
		Stdin:      []byte(effectivePrompt),
		Env:        env,
		StdoutPath: rawPath,
		StderrPath: stderrPath,
	})
	result.Duration = model.NewDuration(time.Since(started))
	result.ExitCode = processResult.ExitCode
	parsed, parseErr := readClaudeOutput(rawPath, result.Model)
	applyTelemetry(&result, parsed.Telemetry)
	inspectionError := claudeInspectionError(processResult, parseErr, parsed.Report, result.Reviewer, request.Target, rawPath, stderrPath)
	if inspectionError == nil {
		report := parsed.Report
		attachTarget(&report, result.Reviewer, request.Target)
		result.Status = "completed"
		result.Report = &report
		return result
	}

	inspectionReachedReserve := claudeReachedMaxTurns(rawPath, inspectionError.Error())
	canFinalize := inspectionReachedReserve || processResult.Err == nil
	if canFinalize && reviewCtx.Err() == nil {
		candidate := partialReportCandidate(parsed.Report, checkpointPath)
		if candidate == nil {
			fallback := claudeFinalizationFallback(request, result.Reviewer, inspectionError.Error())
			candidate = &fallback
		} else {
			attachTarget(candidate, result.Reviewer, request.Target)
		}
		finalConfig, promptErr := claudeFinalizationConfig(c.Config, parsed.Telemetry.Usage)
		finalPrompt := ""
		if promptErr == nil {
			finalPrompt, promptErr = claudeFinalizationPrompt(*candidate, inspectionError.Error())
		}
		if promptErr == nil {
			promptErr = persistReviewerPrompt(request.RunDir, result.Reviewer+"-finalization", finalPrompt)
		}
		if promptErr == nil {
			promptErr = preserveClaudeInspectionArtifacts(rawPath, stderrPath)
		}
		if promptErr == nil {
			finalArgs := claudeReviewArgs(finalConfig, request, compactSchema, finalPrompt, c.Config.FinalizationTurns, "")
			finalResult := processx.Run(reviewCtx, processx.Spec{
				Command: path, Args: finalArgs, Dir: request.WorkDir, Stdin: []byte(finalPrompt), Env: env,
				StdoutPath: rawPath, StderrPath: stderrPath,
			})
			result.Duration = model.NewDuration(time.Since(started))
			result.ExitCode = finalResult.ExitCode
			finalOutput, finalParseErr := readClaudeOutput(rawPath, result.Model)
			applyTelemetry(&result, combineReviewerTelemetry(parsed.Telemetry, finalOutput.Telemetry))
			if finalResult.Err == nil && finalParseErr == nil {
				finalReport := finalOutput.Report
				attachTarget(&finalReport, result.Reviewer, request.Target)
				if validationErr := validateReport(finalReport); validationErr == nil {
					if preservationErr := validateClaudeFinalization(*candidate, finalReport); preservationErr == nil {
						result.Status = "completed"
						result.Report = &finalReport
						return result
					} else {
						finalParseErr = preservationErr
					}
				} else {
					finalParseErr = validationErr
				}
			}
			if finalResult.Err != nil {
				result.Error = "Claude finalization failed after the inspection reserve: " + claudeFailure(rawPath, stderrPath, finalResult.Err)
			} else {
				result.Error = "parse Claude reserved-turn finalization: " + finalParseErr.Error()
			}
			classifyFailure(&result, time.Now())
			attachPartialReviewerReport(&result, request, candidate, "Claude could not finalize within its reserved turns.")
			return result
		}
		result.Error = "prepare Claude reserved-turn finalization: " + promptErr.Error()
		attachPartialReviewerReport(&result, request, candidate, "Claude could not start its reserved finalization phase.")
		return result
	}

	result.Error = inspectionError.Error()
	classifyFailure(&result, time.Now())
	if errors.Is(processResult.Err, context.DeadlineExceeded) {
		result.FailureKind = "timeout"
		attachPartialReviewerReport(&result, request, partialReportCandidate(parsed.Report, checkpointPath), "Claude review timed out before producing a complete report.")
	}
	return result
}

func claudeReviewArgs(cfg config.Reviewer, request Request, schema []byte, _ string, maxTurns int, tools string) []string {
	args := []string{
		"-p",
		"--safe-mode",
		"--permission-mode", "dontAsk",
		"--tools", tools,
		"--settings", claudeReviewerSandboxSettings(request.RuntimeDir, request.RecoveryDir),
		"--append-system-prompt", request.Policy,
		"--max-turns", strconv.Itoa(maxTurns),
		"--no-session-persistence",
		"--output-format", "json",
		"--json-schema", string(schema),
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(cfg.MaxBudgetUSD, 'f', -1, 64))
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	return args
}

func claudeInspectionError(processResult processx.Result, parseErr error, report model.ReviewReport, reviewer string, target model.Target, rawPath, stderrPath string) error {
	if processResult.Err != nil {
		return errors.New("Claude review failed: " + claudeFailure(rawPath, stderrPath, processResult.Err))
	}
	if parseErr != nil {
		return fmt.Errorf("parse Claude report: %w", parseErr)
	}
	attachTarget(&report, reviewer, target)
	if err := validateReport(report); err != nil {
		return fmt.Errorf("validate Claude report: %w", err)
	}
	return nil
}

func claudeFinalizationFallback(request Request, reviewer, reason string) model.ReviewReport {
	return model.ReviewReport{
		SchemaVersion: model.SchemaVersion, Reviewer: reviewer,
		BaseSHA: request.Target.BaseSHA, HeadSHA: request.Target.HeadSHA,
		Verdict: "abstain", Summary: "Inspection reached its enforced finalization boundary before a complete report was available.",
		ContextComplete: false, Findings: []model.Finding{}, ReviewedPaths: []string{},
		OmittedPaths:  append([]string(nil), request.ChangedPaths...),
		ResidualRisks: []string{"Inspection stopped before completion: " + strings.TrimSpace(reason)},
	}
}

func claudeFinalizationPrompt(candidate model.ReviewReport, reason string) (string, error) {
	contents, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`CORA reserved-turn finalization phase.

Repository tools are mechanically disabled. Do not investigate, speculate, or add findings. Return only the required structured report, using the inspection evidence below.

- Preserve verdict, context_complete, summary, every finding, and all reviewed_paths, omitted_paths, and residual_risks exactly.
- Finalization cannot upgrade, weaken, or otherwise rewrite the inspection evidence.
- Normalize the evidence to the supplied schema and finish immediately.

Inspection stop reason: %s

Inspection evidence:
%s
`, strings.TrimSpace(reason), contents), nil
}

// validateClaudeFinalization makes the tools-disabled phase a serializer, not
// a second reviewer. The reserved turns may only return the exact inspection
// evidence Cora supplied; changing a verdict, dropping a finding, or narrowing
// an omitted path would otherwise let finalization strengthen an incomplete or
// request-changes result into an approval.
func validateClaudeFinalization(candidate, finalized model.ReviewReport) error {
	if !reflect.DeepEqual(candidate, finalized) {
		return errors.New("finalizer changed the preserved inspection report")
	}
	return nil
}

func claudeFinalizationConfig(cfg config.Reviewer, inspection model.Usage) (config.Reviewer, error) {
	if cfg.MaxBudgetUSD <= 0 {
		return cfg, nil
	}
	if !inspection.APIEquivalentCostKnown || inspection.APIEquivalentCostPartial {
		return config.Reviewer{}, errors.New("cannot enforce the whole-review max_budget_usd because inspection cost telemetry is incomplete")
	}
	remaining := cfg.MaxBudgetUSD - inspection.APIEquivalentCostUSD
	if remaining <= 0 {
		return config.Reviewer{}, fmt.Errorf("inspection exhausted the whole-review max_budget_usd ceiling of $%.2f", cfg.MaxBudgetUSD)
	}
	cfg.MaxBudgetUSD = remaining
	return cfg, nil
}

func preserveClaudeInspectionArtifacts(rawPath, stderrPath string) error {
	for _, artifact := range []struct {
		from string
		to   string
	}{
		{from: rawPath, to: strings.TrimSuffix(rawPath, ".raw.json") + ".inspection.raw.json"},
		{from: stderrPath, to: strings.TrimSuffix(stderrPath, ".stderr.log") + ".inspection.stderr.log"},
	} {
		if err := os.Rename(artifact.from, artifact.to); err != nil {
			return err
		}
	}
	return nil
}

func combineReviewerTelemetry(inspection, finalization reviewerTelemetry) reviewerTelemetry {
	combined := inspection
	if finalization.Model != "" {
		combined.Model = finalization.Model
		combined.ModelSource = finalization.ModelSource
	}
	combined.Usage = combinePhaseUsage(inspection.Usage, finalization.Usage)
	return combined
}

func combinePhaseUsage(inspection, finalization model.Usage) model.Usage {
	combined := inspection
	combined.InputTokens += finalization.InputTokens
	combined.CachedInputTokens += finalization.CachedInputTokens
	combined.OutputTokens += finalization.OutputTokens
	combined.ThinkingTokens += finalization.ThinkingTokens
	combined.Turns += finalization.Turns
	combined.APIEquivalentCostUSD += finalization.APIEquivalentCostUSD
	combined.TurnsKnown, combined.TurnsPartial = combinedMetricAvailability(
		inspection.TurnsKnown, inspection.TurnsPartial, finalization.TurnsKnown, finalization.TurnsPartial,
	)
	combined.ThinkingTokensKnown, combined.ThinkingTokensPartial = combinedMetricAvailability(
		inspection.ThinkingTokensKnown, inspection.ThinkingTokensPartial, finalization.ThinkingTokensKnown, finalization.ThinkingTokensPartial,
	)
	combined.APIEquivalentCostKnown, combined.APIEquivalentCostPartial = combinedMetricAvailability(
		inspection.APIEquivalentCostKnown, inspection.APIEquivalentCostPartial, finalization.APIEquivalentCostKnown, finalization.APIEquivalentCostPartial,
	)
	if inspection.CostSource == finalization.CostSource {
		combined.CostSource = inspection.CostSource
	} else {
		combined.CostSource = strings.Trim(strings.Join([]string{inspection.CostSource, finalization.CostSource}, "; "), "; ")
	}
	return combined
}

func combinedMetricAvailability(firstKnown, firstPartial, secondKnown, secondPartial bool) (known, partial bool) {
	firstAvailable := firstKnown || firstPartial
	secondAvailable := secondKnown || secondPartial
	known = firstKnown && !firstPartial && secondKnown && !secondPartial
	partial = (firstAvailable || secondAvailable) && !known
	return known, partial
}

func claudePrompt(prompt string, cfg config.Reviewer) string {
	toolTurns := cfg.MaxTurns - cfg.FinalizationTurns
	return prompt + fmt.Sprintf(`

CORA turn-budget contract:
- This inspection process is mechanically capped at %d turn(s); finish and return the structured report sooner whenever possible.
- If inspection reaches that cap without a report, CORA starts a separate tools-disabled finalizer capped at the remaining %d reserved turn(s).
- If inspection is incomplete, return a best-effort report with verdict "abstain", context_complete false, every unreviewed path in omitted_paths, and remaining uncertainty in residual_risks.
- The reserved finalizer cannot use repository tools or promote incomplete evidence into approval.
`, toolTurns, cfg.FinalizationTurns)
}

func claudeReachedMaxTurns(path, message string) bool {
	normalized := strings.ToLower(message)
	if strings.Contains(normalized, "max_turns") || strings.Contains(normalized, "max turns") {
		return true
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var envelope struct {
		TerminalReason string `json:"terminal_reason"`
		Subtype        string `json:"subtype"`
	}
	if json.Unmarshal(contents, &envelope) != nil {
		return false
	}
	terminal := strings.ToLower(envelope.TerminalReason + " " + envelope.Subtype)
	return strings.Contains(terminal, "max_turns") || strings.Contains(terminal, "max turns")
}

func usablePartialReport(report model.ReviewReport) *model.ReviewReport {
	if validateReport(report) != nil {
		return nil
	}
	return &report
}

func partialReportCandidate(providerReport model.ReviewReport, checkpointPath string) *model.ReviewReport {
	if report := usablePartialReport(providerReport); report != nil {
		return report
	}
	if report, found := readValidatedCheckpoint(checkpointPath); found {
		// Checkpoints are writable recovery hints, not finalized provider output.
		// Enforce the prompt contract before allowing one into finalization so a
		// modified checkpoint can never become a completed approval.
		if report.Verdict != "abstain" || report.ContextComplete {
			return nil
		}
		return &report
	}
	return nil
}

// attachPartialReviewerReport preserves any valid structured evidence emitted
// before a reviewer was interrupted, while forcing the result to remain
// fail-closed. A partial report can inform the user, but can never contribute
// an approval or claim complete coverage.
func attachPartialReviewerReport(result *model.ReviewerResult, request Request, candidate *model.ReviewReport, reason string) {
	report := model.ReviewReport{
		SchemaVersion: model.SchemaVersion,
		Findings:      []model.Finding{},
		ReviewedPaths: []string{},
		OmittedPaths:  []string{},
		ResidualRisks: []string{},
	}
	if candidate != nil && validateReport(*candidate) == nil {
		report = *candidate
		report.Findings = append([]model.Finding(nil), candidate.Findings...)
		report.ReviewedPaths = append([]string(nil), candidate.ReviewedPaths...)
		report.OmittedPaths = append([]string(nil), candidate.OmittedPaths...)
		report.ResidualRisks = append([]string(nil), candidate.ResidualRisks...)
	}
	attachTarget(&report, result.Reviewer, request.Target)
	report.Verdict = "abstain"
	report.ContextComplete = false
	if strings.TrimSpace(report.Summary) == "" {
		report.Summary = reason
	} else {
		report.Summary = strings.TrimSpace(report.Summary) + " (Partial report; review did not complete.)"
	}
	reviewed := make(map[string]bool, len(report.ReviewedPaths))
	for _, path := range report.ReviewedPaths {
		reviewed[path] = true
	}
	omitted := make(map[string]bool, len(report.OmittedPaths)+len(request.ChangedPaths))
	for _, path := range report.OmittedPaths {
		omitted[path] = true
	}
	for _, path := range request.ChangedPaths {
		if !reviewed[path] && !omitted[path] {
			report.OmittedPaths = append(report.OmittedPaths, path)
			omitted[path] = true
		}
	}
	if strings.TrimSpace(result.Error) != "" && !containsString(report.ResidualRisks, result.Error) {
		report.ResidualRisks = append(report.ResidualRisks, result.Error)
	}
	result.Status = "partial"
	result.Report = &report
	contents, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(request.RunDir, fileStem(result.Reviewer)+".partial.json")
	if preparePrivateFile(path) == nil {
		_ = os.WriteFile(path, append(contents, '\n'), 0o600)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var (
	quotaResetPattern    = regexp.MustCompile(`(?i)\breset(?:s)?(?:\s+at)?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	quotaTimezonePattern = regexp.MustCompile(`\(([A-Za-z][A-Za-z0-9_+\-/]+(?:/[A-Za-z0-9_+\-]+)+)\)`)
	easternTimePattern   = regexp.MustCompile(`(?i)\bET\b`)
)

func classifyFailure(result *model.ReviewerResult, now time.Time) {
	normalized := strings.ToLower(result.Error)
	if strings.Contains(normalized, "model") && strings.Contains(normalized, "not supported") {
		result.FailureKind = "unsupported_model"
		result.Retryable = false
		return
	}
	retryAt, quota := QuotaRetryAt(result.Error, now)
	if !quota {
		return
	}
	result.FailureKind = "quota"
	result.Retryable = true
	if !retryAt.IsZero() {
		result.RetryAt = &retryAt
	}
}

// readCodexPartialReport returns the latest valid structured report Codex
// emitted before termination. Codex mirrors agent messages to its JSONL event
// stream before output-last-message is finalized, so the event stream is an
// important recovery source when a timeout interrupts CLI shutdown.
func readCodexPartialReport(checkpointPath, rawPath, eventsPath string) (model.ReviewReport, bool) {
	var latest model.ReviewReport
	found := false
	file, err := os.Open(eventsPath)
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var event struct {
				Type string `json:"type"`
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "item.completed" || event.Item.Type != "agent_message" {
				continue
			}
			report, parseErr := parseStructuredReportText(event.Item.Text)
			if parseErr == nil && validateReport(report) == nil {
				latest, found = report, true
			}
		}
	}
	// The private recovery checkpoint is written after a finding changes, so it is newer
	// than any earlier progress message and survives until the provider returns.
	if report, checkpointFound := readValidatedCheckpoint(checkpointPath); checkpointFound {
		latest, found = report, true
	}
	// A finalized output file is newer and more authoritative than a mirrored
	// event message or checkpoint when it survived termination.
	if report, rawFound := readValidatedReport(rawPath); rawFound {
		latest, found = report, true
	}
	return latest, found
}

func readValidatedReport(path string) (model.ReviewReport, bool) {
	if strings.TrimSpace(path) == "" {
		return model.ReviewReport{}, false
	}
	report, err := readReport(path)
	if err != nil || validateReport(report) != nil {
		return model.ReviewReport{}, false
	}
	return report, true
}

func readValidatedCheckpoint(path string) (model.ReviewReport, bool) {
	if strings.TrimSpace(path) == "" {
		return model.ReviewReport{}, false
	}
	contents, err := readRecoveryCheckpoint(path)
	if err != nil {
		return model.ReviewReport{}, false
	}
	var report model.ReviewReport
	if json.Unmarshal(contents, &report) != nil || validateReport(report) != nil {
		return model.ReviewReport{}, false
	}
	return report, true
}

func parseStructuredReportText(value string) (model.ReviewReport, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") && strings.HasSuffix(value, "```") {
		lines := strings.Split(value, "\n")
		if len(lines) >= 3 {
			value = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	var report model.ReviewReport
	if err := json.Unmarshal([]byte(value), &report); err != nil {
		return model.ReviewReport{}, err
	}
	return report, nil
}

func codexFailure(eventsPath, stderrPath string, processErr error) string {
	file, err := os.Open(eventsPath)
	if err == nil {
		defer file.Close()
		var last string
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var event map[string]any
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			typeName, _ := event["type"].(string)
			if typeName != "error" && typeName != "turn.failed" {
				continue
			}
			message := findString(event, "message")
			if decoded := decodeProviderError(message); decoded != "" {
				last = decoded
			}
		}
		if strings.TrimSpace(last) != "" {
			return last
		}
	}
	return stderrFailure(stderrPath, processErr)
}

func decodeProviderError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &envelope) == nil && strings.TrimSpace(envelope.Error.Message) != "" {
		return strings.TrimSpace(envelope.Error.Message)
	}
	return message
}

// QuotaRetryAt recognizes provider quota failures and extracts their reset
// time when one is present. The timestamp is interpreted relative to the time
// the failure occurred, including an IANA location emitted by the provider.
func QuotaRetryAt(message string, observedAt time.Time) (time.Time, bool) {
	normalized := strings.ToLower(message)
	if !strings.Contains(normalized, "quota") && !strings.Contains(normalized, "usage limit") && !strings.Contains(normalized, "session limit") && !strings.Contains(normalized, "rate limit") && !strings.Contains(normalized, "hit your limit") {
		return time.Time{}, false
	}
	match := quotaResetPattern.FindStringSubmatch(message)
	if len(match) != 4 {
		return time.Time{}, true
	}
	hour, _ := strconv.Atoi(match[1])
	minute := 0
	if match[2] != "" {
		minute, _ = strconv.Atoi(match[2])
	}
	if hour < 1 || hour > 12 || minute < 0 || minute > 59 {
		return time.Time{}, true
	}
	if strings.EqualFold(match[3], "pm") && hour != 12 {
		hour += 12
	}
	if strings.EqualFold(match[3], "am") && hour == 12 {
		hour = 0
	}
	location := observedAt.Location()
	if timezoneMatch := quotaTimezonePattern.FindStringSubmatch(message); len(timezoneMatch) == 2 {
		if parsedLocation, err := time.LoadLocation(timezoneMatch[1]); err == nil {
			location = parsedLocation
		}
	} else if easternTimePattern.MatchString(message) {
		if eastern, err := time.LoadLocation("America/New_York"); err == nil {
			location = eastern
		}
	}
	localObservedAt := observedAt.In(location)
	retryAt := time.Date(localObservedAt.Year(), localObservedAt.Month(), localObservedAt.Day(), hour, minute, 0, 0, location)
	if !retryAt.After(observedAt) {
		// Preserve the provider's displayed local wall-clock time across a DST
		// transition; adding 24 hours can turn a promised 4am reset into 3am/5am.
		retryAt = retryAt.AddDate(0, 0, 1)
	}
	return retryAt, true
}

type reviewerTelemetry struct {
	Model       string
	ModelSource string
	Usage       model.Usage
}

func applyTelemetry(result *model.ReviewerResult, telemetry reviewerTelemetry) {
	if telemetry.Model != "" {
		result.Model = telemetry.Model
		result.ModelSource = telemetry.ModelSource
	}
	result.Usage = telemetry.Usage
}

func normalizedAttempt(attempt int) int {
	if attempt < 1 {
		return 1
	}
	return attempt
}

func codexFallbackPaths() []string {
	paths := []string{"/Applications/ChatGPT.app/Contents/Resources/codex"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, "Applications", "ChatGPT.app", "Contents", "Resources", "codex"))
	}
	return paths
}

func preparePrivateFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func codexUsesChatGPT(stdout, stderr []byte) bool {
	authText := strings.ToLower(string(stdout) + "\n" + string(stderr))
	return strings.Contains(authText, "chatgpt")
}

func lookPathWithFallback(command string, fallbacks []string) (string, error) {
	path, err := exec.LookPath(command)
	if err == nil {
		return path, nil
	}
	if filepath.Base(command) != command {
		return "", err
	}
	for _, fallback := range fallbacks {
		if path, fallbackErr := exec.LookPath(fallback); fallbackErr == nil {
			return path, nil
		}
	}
	return "", err
}

func schemaForClaude(schema []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, err
	}
	// Claude Code currently validates structured-output schemas with a dialect
	// that rejects the Draft 2020-12 declaration. CORA's schema only uses
	// keywords supported by Claude's validator, so omit the dialect annotation
	// for this adapter while preserving the canonical schema for Codex and the
	// audit record.
	delete(document, "$schema")
	return json.Marshal(document)
}

type claudeOutput struct {
	Report    model.ReviewReport
	Telemetry reviewerTelemetry
}

func readClaudeReport(path string) (model.ReviewReport, error) {
	output, err := readClaudeOutput(path, "")
	return output.Report, err
}

func readClaudeOutput(path, fallbackModel string) (claudeOutput, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return claudeOutput{}, err
	}
	var envelope struct {
		Type             string          `json:"type"`
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Subtype          string          `json:"subtype"`
		TerminalReason   string          `json:"terminal_reason"`
		Errors           []string        `json:"errors"`
	}
	telemetry := claudeTelemetry(contents, fallbackModel)
	if err := json.Unmarshal(contents, &envelope); err == nil && (envelope.Type != "" || envelope.IsError || len(envelope.StructuredOutput) > 0 || envelope.Result != "") {
		var report model.ReviewReport
		var reportErr error
		if len(envelope.StructuredOutput) > 0 && string(envelope.StructuredOutput) != "null" {
			reportErr = json.Unmarshal(envelope.StructuredOutput, &report)
		} else if strings.TrimSpace(envelope.Result) != "" {
			reportErr = json.Unmarshal([]byte(envelope.Result), &report)
		}
		output := claudeOutput{Report: report, Telemetry: telemetry}
		if envelope.IsError {
			return output, errors.New(firstNonEmpty(strings.Join(envelope.Errors, "; "), envelope.Result, envelope.TerminalReason, envelope.Subtype, "Claude returned an error result"))
		}
		if reportErr != nil {
			return output, reportErr
		}
		if len(envelope.StructuredOutput) == 0 && strings.TrimSpace(envelope.Result) == "" {
			return output, errors.New("Claude result did not contain a structured report")
		}
		return output, nil
	}
	var report model.ReviewReport
	if err := json.Unmarshal(contents, &report); err != nil {
		return claudeOutput{Telemetry: telemetry}, err
	}
	return claudeOutput{Report: report, Telemetry: telemetry}, nil
}

func readCodexTelemetry(path, fallbackModel string) (reviewerTelemetry, error) {
	file, err := os.Open(path)
	if err != nil {
		return reviewerTelemetry{}, err
	}
	defer file.Close()

	telemetry := reviewerTelemetry{Model: fallbackModel}
	if fallbackModel != "" {
		telemetry.ModelSource = "configured"
	}
	var completed, fallback model.Usage
	completedUsage := false
	fallbackUsage := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if resolved := findString(event, "resolved_model", "model_name", "model"); resolved != "" {
			telemetry.Model = resolved
			telemetry.ModelSource = "provider"
		}
		usage, found := extractUsage(event)
		if typeName, _ := event["type"].(string); typeName == "turn.completed" {
			completed.Turns++
			completed.TurnsKnown = true
			if found {
				completed = addUsage(completed, usage, completedUsage)
				completedUsage = true
			}
		} else if found && usageMagnitude(usage) >= usageMagnitude(fallback) {
			fallback = usage
			fallbackUsage = true
		}
	}
	if err := scanner.Err(); err != nil {
		return telemetry, err
	}
	if completedUsage {
		telemetry.Usage = completed
	} else {
		telemetry.Usage = fallback
		telemetry.Usage.Turns = completed.Turns
		telemetry.Usage.TurnsKnown = completed.TurnsKnown
	}
	if completedUsage || fallbackUsage {
		applyCodexPrice(&telemetry.Usage, telemetry.Model)
	}
	return telemetry, nil
}

func claudeTelemetry(contents []byte, fallbackModel string) reviewerTelemetry {
	telemetry := reviewerTelemetry{Model: fallbackModel}
	if fallbackModel != "" {
		telemetry.ModelSource = "configured"
	}
	var document map[string]any
	if json.Unmarshal(contents, &document) != nil {
		return telemetry
	}
	if resolved := stringValue(document, "resolved_model", "resolvedModel"); resolved != "" {
		telemetry.Model = resolved
		telemetry.ModelSource = "provider"
	}
	if turns, found := numberValueKnown(document, "num_turns", "numTurns"); found {
		telemetry.Usage.Turns = int(turns)
		telemetry.Usage.TurnsKnown = true
	}

	if modelUsage, ok := mapValue(document, "modelUsage", "model_usage"); ok {
		var bestModel string
		var bestMagnitude int64
		aggregatedUsage := false
		for name, raw := range modelUsage {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			usage, found := claudeUsageFromMap(entry)
			if found {
				telemetry.Usage = addUsage(telemetry.Usage, usage, aggregatedUsage)
				aggregatedUsage = true
			}
			magnitude := usageMagnitude(usage)
			if magnitude > bestMagnitude || bestModel == "" {
				bestModel = name
				bestMagnitude = magnitude
			}
		}
		if telemetry.ModelSource != "provider" && bestModel != "" {
			telemetry.Model = bestModel
			telemetry.ModelSource = "provider"
		}
	} else if usage, found := extractClaudeUsage(document); found {
		turns := telemetry.Usage.Turns
		turnsKnown := telemetry.Usage.TurnsKnown
		telemetry.Usage = usage
		telemetry.Usage.Turns = turns
		telemetry.Usage.TurnsKnown = turnsKnown
	}
	if cost, found := numberValueKnown(document, "total_cost_usd", "totalCostUSD"); found {
		telemetry.Usage.APIEquivalentCostUSD = cost
		telemetry.Usage.APIEquivalentCostKnown = true
		telemetry.Usage.CostSource = "claude-code-result.total_cost_usd"
	} else if modelUsage, ok := mapValue(document, "modelUsage", "model_usage"); ok {
		var total float64
		foundCost := false
		for _, raw := range modelUsage {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if cost, found := numberValueKnown(entry, "costUSD", "cost_usd"); found {
				total += cost
				foundCost = true
			}
		}
		if foundCost {
			telemetry.Usage.APIEquivalentCostUSD = total
			telemetry.Usage.APIEquivalentCostKnown = true
			telemetry.Usage.CostSource = "claude-code-result.modelUsage.costUSD"
		}
	}
	return telemetry
}

func extractUsage(value any) (model.Usage, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return model.Usage{}, false
	}
	if usage, found := usageFromMap(object); found {
		return usage, true
	}
	var best model.Usage
	found := false
	for _, nested := range object {
		switch value := nested.(type) {
		case map[string]any:
			usage, nestedFound := extractUsage(value)
			if nestedFound && (!found || usageMagnitude(usage) > usageMagnitude(best)) {
				best, found = usage, true
			}
		case []any:
			for _, item := range value {
				usage, nestedFound := extractUsage(item)
				if nestedFound && (!found || usageMagnitude(usage) > usageMagnitude(best)) {
					best, found = usage, true
				}
			}
		}
	}
	return best, found
}

func usageFromMap(object map[string]any) (model.Usage, bool) {
	input, inputKnown := intValueKnown(object, "input_tokens", "inputTokens")
	cached, cachedKnown := intValueKnown(object, "cached_input_tokens", "cachedInputTokens", "cache_read_input_tokens", "cacheReadInputTokens")
	_, creationKnown := intValueKnown(object, "cache_creation_input_tokens", "cacheCreationInputTokens")
	output, outputKnown := intValueKnown(object, "output_tokens", "outputTokens")
	thinking, thinkingKnown := intValueKnown(object,
		"reasoning_tokens", "reasoningTokens",
		"reasoning_output_tokens", "reasoningOutputTokens",
		"thinking_tokens", "thinkingTokens",
	)
	if !thinkingKnown {
		if details, ok := mapValue(object, "output_tokens_details", "outputTokensDetails"); ok {
			thinking, thinkingKnown = intValueKnown(details,
				"reasoning_tokens", "reasoningTokens",
				"reasoning_output_tokens", "reasoningOutputTokens",
				"thinking_tokens", "thinkingTokens",
			)
		}
	}
	if !inputKnown && !cachedKnown && !creationKnown && !outputKnown && !thinkingKnown {
		return model.Usage{}, false
	}
	return model.Usage{
		InputTokens:         input,
		CachedInputTokens:   cached,
		OutputTokens:        output,
		ThinkingTokens:      thinking,
		ThinkingTokensKnown: thinkingKnown,
	}, true
}

func claudeUsageFromMap(object map[string]any) (model.Usage, bool) {
	usage, found := usageFromMap(object)
	if !found {
		return model.Usage{}, false
	}
	cached, _ := intValueKnown(object, "cached_input_tokens", "cachedInputTokens", "cache_read_input_tokens", "cacheReadInputTokens")
	cacheCreation, _ := intValueKnown(object, "cache_creation_input_tokens", "cacheCreationInputTokens")
	usage.InputTokens += cached + cacheCreation
	return usage, true
}

func extractClaudeUsage(value any) (model.Usage, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return model.Usage{}, false
	}
	if usage, found := claudeUsageFromMap(object); found {
		return usage, true
	}
	var best model.Usage
	found := false
	for _, nested := range object {
		switch value := nested.(type) {
		case map[string]any:
			usage, nestedFound := extractClaudeUsage(value)
			if nestedFound && (!found || usageMagnitude(usage) > usageMagnitude(best)) {
				best, found = usage, true
			}
		case []any:
			for _, item := range value {
				usage, nestedFound := extractClaudeUsage(item)
				if nestedFound && (!found || usageMagnitude(usage) > usageMagnitude(best)) {
					best, found = usage, true
				}
			}
		}
	}
	return best, found
}

func addUsage(left, right model.Usage, hasLeftUsage bool) model.Usage {
	turns := left.Turns
	turnsKnown := left.TurnsKnown
	left.InputTokens += right.InputTokens
	left.CachedInputTokens += right.CachedInputTokens
	left.OutputTokens += right.OutputTokens
	left.ThinkingTokens += right.ThinkingTokens
	if !hasLeftUsage {
		left.ThinkingTokensKnown = right.ThinkingTokensKnown && !right.ThinkingTokensPartial
		left.ThinkingTokensPartial = right.ThinkingTokensPartial
	} else {
		anyThinkingKnown := left.ThinkingTokensKnown || left.ThinkingTokensPartial || right.ThinkingTokensKnown || right.ThinkingTokensPartial
		allThinkingKnown := left.ThinkingTokensKnown && !left.ThinkingTokensPartial && right.ThinkingTokensKnown && !right.ThinkingTokensPartial
		left.ThinkingTokensKnown = allThinkingKnown
		left.ThinkingTokensPartial = anyThinkingKnown && !allThinkingKnown
	}
	left.Turns = turns
	left.TurnsKnown = turnsKnown
	return left
}

func usageMagnitude(usage model.Usage) int64 {
	return usage.InputTokens + usage.OutputTokens + usage.ThinkingTokens
}

func applyCodexPrice(usage *model.Usage, modelName string) {
	type prices struct{ input, cached, output float64 }
	var rate prices
	normalized := strings.ToLower(modelName)
	switch {
	case normalized == "gpt-5.6" || strings.HasPrefix(normalized, "gpt-5.6-sol"):
		rate = prices{input: 4, cached: 0.4, output: 20}
	default:
		return
	}
	if usage.InputTokens > 272_000 {
		rate.input *= 2
		rate.cached *= 2
		rate.output *= 1.5
	}
	uncached := usage.InputTokens - usage.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	usage.APIEquivalentCostUSD = (float64(uncached)*rate.input + float64(usage.CachedInputTokens)*rate.cached + float64(usage.OutputTokens)*rate.output) / 1_000_000
	usage.APIEquivalentCostKnown = true
	usage.CostSource = "OpenAI GPT-5.6 pricing observed 2026-08-25 (USD/MTok: input=4, cached=0.4, output=20; long-context multipliers apply)"
}

func findString(value any, keys ...string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if found := stringValue(object, keys...); found != "" {
		return found
	}
	for _, nested := range object {
		switch value := nested.(type) {
		case map[string]any:
			if found := findString(value, keys...); found != "" {
				return found
			}
		case []any:
			for _, item := range value {
				if found := findString(item, keys...); found != "" {
					return found
				}
			}
		}
	}
	return ""
}

func mapValue(object map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := object[key].(map[string]any); ok {
			return value, true
		}
	}
	return nil, false
}

func stringValue(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok {
			return value
		}
	}
	return ""
}

func numberValueKnown(object map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch value := object[key].(type) {
		case float64:
			return value, true
		case json.Number:
			parsed, err := value.Float64()
			return parsed, err == nil
		}
	}
	return 0, false
}

func intValueKnown(object map[string]any, keys ...string) (int64, bool) {
	value, found := numberValueKnown(object, keys...)
	if !found || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return int64(value), true
}

func fileStem(name string) string {
	var stem strings.Builder
	for _, character := range strings.ToLower(name) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			stem.WriteRune(character)
		} else {
			stem.WriteByte('-')
		}
	}
	return strings.Trim(stem.String(), "-")
}

func claudeFailure(rawPath, stderrPath string, processErr error) string {
	if _, err := readClaudeReport(rawPath); err != nil && !errors.Is(err, os.ErrNotExist) && err.Error() != "unexpected end of JSON input" {
		return err.Error()
	}
	return stderrFailure(stderrPath, processErr)
}

func stderrFailure(stderrPath string, processErr error) string {
	stderr, _ := os.ReadFile(stderrPath)
	return firstNonEmpty(string(stderr), processErr.Error())
}

func readReport(path string) (model.ReviewReport, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return model.ReviewReport{}, err
	}
	var report model.ReviewReport
	if err := json.Unmarshal(contents, &report); err != nil {
		return model.ReviewReport{}, err
	}
	return report, nil
}

func attachTarget(report *model.ReviewReport, reviewer string, target model.Target) {
	report.Reviewer = reviewer
	report.BaseSHA = target.BaseSHA
	report.HeadSHA = target.HeadSHA
}

func validateReport(report model.ReviewReport) error {
	if report.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", report.SchemaVersion)
	}
	switch report.Verdict {
	case "approve", "request_changes", "abstain":
	default:
		return fmt.Errorf("invalid verdict %q", report.Verdict)
	}
	for i, finding := range report.Findings {
		switch finding.Severity {
		case "blocker", "major", "minor", "note":
		default:
			return fmt.Errorf("finding %d has invalid severity %q", i, finding.Severity)
		}
		if finding.Confidence < 0 || finding.Confidence > 1 {
			return fmt.Errorf("finding %d has invalid confidence", i)
		}
		if strings.TrimSpace(finding.ID) == "" || strings.TrimSpace(finding.Claim) == "" || strings.TrimSpace(finding.Evidence) == "" {
			return fmt.Errorf("finding %d is missing required evidence", i)
		}
		switch finding.Disposition {
		case "", "confirmed", "demoted", "disproved", "uncertain":
		default:
			return fmt.Errorf("finding %d has invalid disposition %q", i, finding.Disposition)
		}
		if finding.Reachability != nil {
			if !model.ValidReachabilityStatus(finding.Reachability.Status) {
				return fmt.Errorf("finding %d has invalid reachability status %q", i, finding.Reachability.Status)
			}
		}
		if finding.Severity == "blocker" || finding.Severity == "major" {
			if finding.Reachability == nil || finding.Reachability.Status != model.ReachabilityDemonstrated || strings.TrimSpace(finding.Reachability.Trigger) == "" || len(finding.Reachability.Path) == 0 || strings.TrimSpace(finding.Reachability.Impact) == "" {
				return fmt.Errorf("finding %d does not demonstrate trigger-to-impact reachability", i)
			}
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown error"
}
