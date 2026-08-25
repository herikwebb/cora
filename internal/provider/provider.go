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
	Target          model.Target
	RunDir          string
	SchemaPath      string
	Schema          []byte
	Prompt          string
	Policy          string
	Timeout         time.Duration
	AllowAPIBilling bool
	Attempt         int
}

type Adapter interface {
	Name() string
	Provider() string
	Review(context.Context, Request) model.ReviewerResult
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
	Config config.Reviewer
}

func (Codex) Name() string     { return "codex" }
func (Codex) Provider() string { return "codex" }

func (c Codex) Review(parent context.Context, request Request) model.ReviewerResult {
	started := time.Now()
	result := model.ReviewerResult{
		Reviewer: c.Name(), Status: "incomplete", Tool: c.Config.Command, Attempt: normalizedAttempt(request.Attempt),
		Model: c.Config.Model, ModelSource: "configured", Effort: c.Config.Effort,
	}
	env := processx.ReviewerEnvironment(request.AllowAPIBilling)

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
	args := codexReviewArgs(c.Config, request, rawPath)
	stderrPath := filepath.Join(request.RunDir, "codex.stderr.log")

	reviewCtx, cancelReview := context.WithTimeout(parent, request.Timeout)
	eventsPath := filepath.Join(request.RunDir, "codex.events.jsonl")
	processResult := processx.Run(reviewCtx, processx.Spec{
		Command:    path,
		Args:       args,
		Dir:        request.WorkDir,
		Stdin:      []byte(request.Prompt),
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
		result.Error = "Codex review failed: " + stderrFailure(stderrPath, processResult.Err)
		classifyFailure(&result, time.Now())
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
		"--sandbox", "read-only",
		"--cd", request.WorkDir,
		"--add-dir", request.RepoRoot,
		"--skip-git-repo-check",
		"--ignore-rules",
		"--ephemeral",
		"--ignore-user-config",
		"--config", "developer_instructions=" + strconv.Quote(request.Policy),
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

type Claude struct {
	Config          config.Reviewer
	ReviewerName    string
	EscalationCause string
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
	env := processx.ReviewerEnvironment(request.AllowAPIBilling)

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
	args := []string{
		"-p",
		"--safe-mode",
		"--permission-mode", "plan",
		"--tools", "Read,Glob,Grep",
		"--append-system-prompt", request.Policy,
		"--add-dir", request.RepoRoot,
		"--add-dir", request.RunDir,
		"--max-turns", strconv.Itoa(c.Config.MaxTurns),
		"--no-session-persistence",
		"--output-format", "json",
		"--json-schema", string(compactSchema),
	}
	if c.Config.Model != "" {
		args = append(args, "--model", c.Config.Model)
	}
	if c.Config.Effort != "" {
		args = append(args, "--effort", c.Config.Effort)
	}
	args = append(args, request.Prompt)

	rawPath := filepath.Join(request.RunDir, fileStem(c.Name())+".raw.json")
	stderrPath := filepath.Join(request.RunDir, fileStem(c.Name())+".stderr.log")
	reviewCtx, cancelReview := context.WithTimeout(parent, request.Timeout)
	processResult := processx.Run(reviewCtx, processx.Spec{
		Command:    path,
		Args:       args,
		Dir:        request.WorkDir,
		Env:        env,
		StdoutPath: rawPath,
		StderrPath: stderrPath,
	})
	cancelReview()
	result.Duration = model.NewDuration(time.Since(started))
	result.ExitCode = processResult.ExitCode
	parsed, parseErr := readClaudeOutput(rawPath, result.Model)
	applyTelemetry(&result, parsed.Telemetry)
	if processResult.Err != nil {
		result.Error = "Claude review failed: " + claudeFailure(rawPath, stderrPath, processResult.Err)
		classifyFailure(&result, time.Now())
		return result
	}
	if parseErr != nil {
		result.Error = "parse Claude report: " + parseErr.Error()
		classifyFailure(&result, time.Now())
		return result
	}
	report := parsed.Report
	attachTarget(&report, result.Reviewer, request.Target)
	if err := validateReport(report); err != nil {
		result.Error = "validate Claude report: " + err.Error()
		return result
	}
	result.Status = "completed"
	result.Report = &report
	return result
}

var (
	quotaResetPattern    = regexp.MustCompile(`(?i)reset(?:s)?(?:\s+at)?\s+(\d{1,2}):(\d{2})\s*(am|pm)`)
	quotaTimezonePattern = regexp.MustCompile(`\(([A-Za-z][A-Za-z0-9_+\-/]+(?:/[A-Za-z0-9_+\-]+)+)\)`)
	easternTimePattern   = regexp.MustCompile(`(?i)\bET\b`)
)

func classifyFailure(result *model.ReviewerResult, now time.Time) {
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
	minute, _ := strconv.Atoi(match[2])
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
		retryAt = retryAt.Add(24 * time.Hour)
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
		if envelope.IsError {
			return claudeOutput{Telemetry: telemetry}, errors.New(firstNonEmpty(strings.Join(envelope.Errors, "; "), envelope.Result, envelope.TerminalReason, envelope.Subtype, "Claude returned an error result"))
		}
		if len(envelope.StructuredOutput) > 0 && string(envelope.StructuredOutput) != "null" {
			var report model.ReviewReport
			if err := json.Unmarshal(envelope.StructuredOutput, &report); err != nil {
				return claudeOutput{Telemetry: telemetry}, err
			}
			return claudeOutput{Report: report, Telemetry: telemetry}, nil
		}
		var report model.ReviewReport
		if err := json.Unmarshal([]byte(envelope.Result), &report); err != nil {
			return claudeOutput{Telemetry: telemetry}, err
		}
		return claudeOutput{Report: report, Telemetry: telemetry}, nil
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
				completed = addUsage(completed, usage)
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
		for name, raw := range modelUsage {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			usage, found := claudeUsageFromMap(entry)
			if found {
				telemetry.Usage = addUsage(telemetry.Usage, usage)
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
	thinking, thinkingKnown := intValueKnown(object, "reasoning_tokens", "reasoningTokens", "thinking_tokens", "thinkingTokens")
	if !thinkingKnown {
		if details, ok := mapValue(object, "output_tokens_details", "outputTokensDetails"); ok {
			thinking, thinkingKnown = intValueKnown(details, "reasoning_tokens", "reasoningTokens")
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

func addUsage(left, right model.Usage) model.Usage {
	turns := left.Turns
	turnsKnown := left.TurnsKnown
	left.InputTokens += right.InputTokens
	left.CachedInputTokens += right.CachedInputTokens
	left.OutputTokens += right.OutputTokens
	left.ThinkingTokens += right.ThinkingTokens
	left.ThinkingTokensKnown = left.ThinkingTokensKnown || right.ThinkingTokensKnown
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
