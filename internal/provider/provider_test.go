package provider

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestCodexReviewArgsUseGenericExecForAuditedPrompt(t *testing.T) {
	request := Request{
		RepoRoot:   "/tmp/repo",
		WorkDir:    "/tmp/repo",
		RuntimeDir: "/tmp/cora-runtime",
		Target:     model.Target{Mode: "branch", BaseSHA: "base-sha"},
		SchemaPath: "/tmp/schema.json",
		Policy:     "trusted policy",
	}
	got := codexReviewArgs(config.Reviewer{Model: "gpt-5.6-sol", Effort: "high"}, request, "/tmp/result.json")
	want := []string{
		"exec", "--sandbox", "workspace-write",
		"--cd", "/tmp/repo",
		"--skip-git-repo-check", "--ignore-rules",
		"--ephemeral", "--ignore-user-config",
		"--config", `developer_instructions="trusted policy"`,
		"--add-dir", "/tmp/cora-runtime",
		"--model", "gpt-5.6-sol",
		"--config", `model_reasoning_effort="high"`,
		"--output-schema", "/tmp/schema.json",
		"--json", "--output-last-message", "/tmp/result.json", "-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex args = %v, want %v", got, want)
	}
}

func TestPrepareReviewerPromptAddsPrivateLowOverheadCheckpoint(t *testing.T) {
	runtimeDir := t.TempDir()
	recoveryDir := t.TempDir()
	runDir := t.TempDir()
	prompt, checkpointPath, err := prepareReviewerPrompt(Request{RuntimeDir: runtimeDir, RecoveryDir: recoveryDir}, "claude", "review this")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Only after you confirm at least one finding",
		"Do not write checkpoints for suspicions or when there are no confirmed findings",
		checkpointPath,
		`verdict "abstain" and context_complete false`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("checkpoint prompt does not contain %q:\n%s", want, prompt)
		}
	}
	if info, statErr := os.Stat(checkpointPath); statErr != nil {
		t.Fatal(statErr)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint permissions = %#o", info.Mode().Perm())
	}
	if filepath.Dir(checkpointPath) != recoveryDir || strings.HasPrefix(checkpointPath, runtimeDir+string(filepath.Separator)) {
		t.Fatalf("checkpoint %q was not isolated from reviewer runtime %q", checkpointPath, runtimeDir)
	}
	if err := persistReviewerPrompt(runDir, "claude", prompt); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(runDir, "claude.effective-prompt.md")
	contents, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != prompt {
		t.Fatalf("effective prompt record differs from executed prompt")
	}
	if info, err := os.Stat(recordPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("effective prompt permissions = %#o", info.Mode().Perm())
	}
}

func TestClaudeReviewerSettingsRequireStrictSandbox(t *testing.T) {
	var settings struct {
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Filesystem               struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
			Network struct {
				DeniedDomains   []string `json:"deniedDomains"`
				StrictAllowlist bool     `json:"strictAllowlist"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(claudeReviewerSandboxSettings("/tmp/cora-runtime", "/tmp/cora-recovery")), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable || !settings.Sandbox.AutoAllowBashIfSandboxed || settings.Sandbox.AllowUnsandboxedCommands {
		t.Fatalf("Claude sandbox settings = %#v", settings.Sandbox)
	}
	if !reflect.DeepEqual(settings.Sandbox.Filesystem.AllowWrite, []string{"/tmp/cora-runtime", "/tmp/cora-recovery"}) || !reflect.DeepEqual(settings.Sandbox.Network.DeniedDomains, []string{"*"}) || !settings.Sandbox.Network.StrictAllowlist {
		t.Fatalf("Claude sandbox boundaries = %#v", settings.Sandbox)
	}
}

func TestCodexFixArgsUseAuditedWorkspaceWriteMode(t *testing.T) {
	request := FixRequest{RepoRoot: "/tmp/repo", Policy: "trusted auto-fix policy"}
	got := codexFixArgs(config.AutoFix{Model: "gpt-5.6-sol", Effort: "high"}, request, "/tmp/last.txt")
	want := []string{
		"exec", "--sandbox", "workspace-write", "--cd", "/tmp/repo",
		"--skip-git-repo-check", "--ignore-rules", "--ephemeral", "--ignore-user-config",
		"--config", `developer_instructions="trusted auto-fix policy"`,
		"--model", "gpt-5.6-sol", "--config", `model_reasoning_effort="high"`,
		"--json", "--output-last-message", "/tmp/last.txt", "-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex fix args = %v, want %v", got, want)
	}
}

func TestDescribeAdapterReturnsConfiguredReviewMetadata(t *testing.T) {
	tests := []struct {
		name    string
		adapter Adapter
		want    AdapterDescriptor
	}{
		{
			name: "codex value",
			adapter: Codex{Config: config.Reviewer{Model: "gpt-5.6-sol", Effort: "high"},
				EscalationCause: "security_sensitive"},
			want: AdapterDescriptor{Model: "gpt-5.6-sol", Effort: "high", EscalationCause: "security_sensitive"},
		},
		{
			name: "claude value",
			adapter: Claude{Config: config.Reviewer{Model: "fable", Effort: "high"},
				EscalationCause: "blocking_cross_examination"},
			want: AdapterDescriptor{Model: "fable", Effort: "high", EscalationCause: "blocking_cross_examination"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DescribeAdapter(test.adapter); got != test.want {
				t.Fatalf("descriptor = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestClassifyQuotaFailureExtractsRetryTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 30, 0, 0, time.FixedZone("ET", -4*60*60))
	result := model.ReviewerResult{Error: "You've hit your usage limit; resets 11:50am"}
	classifyFailure(&result, now)
	if result.FailureKind != "quota" || !result.Retryable || result.RetryAt == nil {
		t.Fatalf("quota classification = %#v", result)
	}
	want := time.Date(2026, 8, 25, 11, 50, 0, 0, now.Location())
	if !result.RetryAt.Equal(want) {
		t.Fatalf("retry time = %s, want %s", result.RetryAt, want)
	}
}

func TestCodexFailureUsesProviderErrorEvent(t *testing.T) {
	directory := t.TempDir()
	events := filepath.Join(directory, "events.jsonl")
	contents := `{"type":"error","message":"Model metadata for gpt-5.6 not found"}
{"type":"turn.failed","message":"{\"error\":{\"message\":\"The 'gpt-5.6' model is not supported when using Codex with a ChatGPT account.\"}}"}
`
	if err := os.WriteFile(events, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	got := codexFailure(events, filepath.Join(directory, "stderr.log"), errors.New("exit status 1"))
	want := "The 'gpt-5.6' model is not supported when using Codex with a ChatGPT account."
	if got != want {
		t.Fatalf("failure = %q, want %q", got, want)
	}
}

func TestValidateReportRequiresReachabilityForBlockingFindings(t *testing.T) {
	report := model.ReviewReport{
		SchemaVersion: model.SchemaVersion,
		Verdict:       "request_changes",
		Findings: []model.Finding{{
			ID: "major-1", Severity: "major", Confidence: 0.9,
			Claim: "Untrusted input reaches the command", Evidence: "handler calls execute", SuggestedFix: "validate the input",
		}},
	}
	if err := validateReport(report); err == nil || !strings.Contains(err.Error(), "trigger-to-impact reachability") {
		t.Fatalf("missing reachability error = %v", err)
	}

	report.Findings[0].Reachability = &model.Reachability{
		Status: "demonstrated", Trigger: "an authenticated request supplies command",
		Path:   []string{"handler.go:20 accepts command", "runner.go:45 passes command to exec"},
		Impact: "the process executes attacker-selected input",
	}
	if err := validateReport(report); err != nil {
		t.Fatalf("demonstrated reachability rejected: %v", err)
	}
}

func TestClaudePromptReservesFinalizationTurns(t *testing.T) {
	got := claudePrompt("review this", config.Reviewer{MaxTurns: 50, FinalizationTurns: 2})
	for _, want := range []string{"no later than turn 48", "final 2 turn(s)", `verdict "abstain"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("Claude prompt does not contain %q:\n%s", want, got)
		}
	}
}

func TestAttachPartialClaudeReportPersistsFailClosedEvidence(t *testing.T) {
	directory := t.TempDir()
	result := model.ReviewerResult{Reviewer: "claude", Status: "incomplete", Error: "Claude review failed: max_turns"}
	request := Request{
		RunDir: directory, Target: model.Target{BaseSHA: "base", HeadSHA: "head"},
		ChangedPaths: []string{"app.go", "auth.go"},
	}
	attachPartialReviewerReport(&result, request, nil, "Claude reached its turn ceiling before producing a complete report.")
	if result.Status != "partial" || result.Report == nil || result.Report.ContextComplete || result.Report.Verdict != "abstain" {
		t.Fatalf("partial result = %#v", result)
	}
	if !reflect.DeepEqual(result.Report.OmittedPaths, request.ChangedPaths) {
		t.Fatalf("omitted paths = %v", result.Report.OmittedPaths)
	}
	path := filepath.Join(directory, "claude.partial.json")
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("partial report permissions = %#o", info.Mode().Perm())
	}
}

func TestAttachPartialReviewerReportRetainsFindingsAndFailsClosed(t *testing.T) {
	directory := t.TempDir()
	result := model.ReviewerResult{Reviewer: "codex", Status: "incomplete", Error: "Codex review failed: timed out: context deadline exceeded"}
	candidate := model.ReviewReport{
		SchemaVersion: model.SchemaVersion, Verdict: "request_changes", ContextComplete: true,
		Summary: "One issue was confirmed before interruption.",
		Findings: []model.Finding{{
			ID: "resource-leak", Severity: "minor", Confidence: 0.91, File: "app.go", Line: 12,
			Claim: "The file remains open on the error path.", Evidence: "app.go:12 returns without closing f.", SuggestedFix: "Defer f.Close after opening it.",
		}},
		ReviewedPaths: []string{"app.go"}, OmittedPaths: []string{}, ResidualRisks: []string{"The success path was not traced."},
	}
	request := Request{
		RunDir: directory, Target: model.Target{BaseSHA: "base", HeadSHA: "head"},
		ChangedPaths: []string{"app.go", "auth.go"},
	}
	attachPartialReviewerReport(&result, request, &candidate, "Codex review timed out before producing a complete report.")
	if result.Status != "partial" || result.Report == nil || result.Report.Verdict != "abstain" || result.Report.ContextComplete {
		t.Fatalf("partial result = %#v", result)
	}
	if len(result.Report.Findings) != 1 || result.Report.Findings[0].ID != "resource-leak" {
		t.Fatalf("partial findings were not retained: %#v", result.Report.Findings)
	}
	if !reflect.DeepEqual(result.Report.OmittedPaths, []string{"auth.go"}) {
		t.Fatalf("partial omitted paths = %v", result.Report.OmittedPaths)
	}
	if !containsString(result.Report.ResidualRisks, result.Error) {
		t.Fatalf("timeout was not recorded as residual risk: %v", result.Report.ResidualRisks)
	}
	path := filepath.Join(directory, "codex.partial.json")
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("partial report permissions = %#o", info.Mode().Perm())
	}
}

func TestValidateReportRejectsInvalidNullableFindingEnums(t *testing.T) {
	report := model.ReviewReport{
		SchemaVersion: model.SchemaVersion, Verdict: "approve", ContextComplete: true,
		Findings: []model.Finding{{
			ID: "minor", Severity: "minor", Confidence: 0.8, File: "app.go", Line: 12,
			Claim: "resource leak", Evidence: "the error return skips Close", SuggestedFix: "defer Close",
			Disposition: "invented",
		}},
	}
	if err := validateReport(report); err == nil || !strings.Contains(err.Error(), "invalid disposition") {
		t.Fatalf("invalid disposition validation error = %v", err)
	}
	report.Findings[0].Disposition = ""
	report.Findings[0].Reachability = &model.Reachability{Status: "invented"}
	if err := validateReport(report); err == nil || !strings.Contains(err.Error(), "invalid reachability status") {
		t.Fatalf("invalid reachability validation error = %v", err)
	}
}

func TestReadCodexPartialReportUsesLatestStructuredAgentMessage(t *testing.T) {
	directory := t.TempDir()
	rawPath := filepath.Join(directory, "codex.raw.json")
	if err := os.WriteFile(rawPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(directory, "codex.events.jsonl")
	contents := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":\"1\",\"verdict\":\"abstain\",\"context_complete\":false,\"summary\":\"inspection started\",\"findings\":[],\"reviewed_paths\":[],\"omitted_paths\":[\"app.go\"],\"residual_risks\":[]}"}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":\"1\",\"verdict\":\"request_changes\",\"context_complete\":false,\"summary\":\"confirmed one issue\",\"findings\":[{\"id\":\"leak\",\"severity\":\"minor\",\"confidence\":0.9,\"file\":\"app.go\",\"line\":12,\"claim\":\"file leak\",\"evidence\":\"error return bypasses close\",\"suggested_fix\":\"defer close\"}],\"reviewed_paths\":[\"app.go\"],\"omitted_paths\":[],\"residual_risks\":[]}"}}
`
	if err := os.WriteFile(eventsPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	report, found := readCodexPartialReport("", rawPath, eventsPath)
	if !found || report.Summary != "confirmed one issue" || len(report.Findings) != 1 || report.Findings[0].ID != "leak" {
		t.Fatalf("recovered report = %#v, found=%v", report, found)
	}
}

func TestReadCodexPartialReportUsesConfirmedFindingCheckpoint(t *testing.T) {
	directory := t.TempDir()
	checkpointPath := filepath.Join(directory, "checkpoint.json")
	checkpoint := `{
  "schema_version":"1",
  "verdict":"abstain",
  "context_complete":false,
  "summary":"confirmed one issue",
  "findings":[{"id":"leak","severity":"minor","confidence":0.9,"file":"app.go","line":12,"claim":"file leak","evidence":"error return bypasses close","suggested_fix":"defer close"}],
  "reviewed_paths":["app.go"],
  "omitted_paths":["auth.go"],
  "residual_risks":[]
}`
	if err := os.WriteFile(checkpointPath, []byte(checkpoint), 0o600); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(directory, "raw.json")
	if err := os.WriteFile(rawPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(directory, "events.jsonl")
	events := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":\"1\",\"verdict\":\"abstain\",\"context_complete\":false,\"summary\":\"inspection started\",\"findings\":[],\"reviewed_paths\":[],\"omitted_paths\":[\"app.go\",\"auth.go\"],\"residual_risks\":[]}"}}
`
	if err := os.WriteFile(eventsPath, []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	report, found := readCodexPartialReport(checkpointPath, rawPath, eventsPath)
	if !found || len(report.Findings) != 1 || report.Findings[0].ID != "leak" {
		t.Fatalf("checkpoint report = %#v, found=%v", report, found)
	}
}

func TestPartialReportCandidateFallsBackToValidatedCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	contents := `{"schema_version":"1","verdict":"abstain","context_complete":false,"summary":"confirmed issue","findings":[{"id":"leak","severity":"minor","confidence":0.9,"file":"app.go","line":12,"claim":"file leak","evidence":"return bypasses close","suggested_fix":"defer close"}],"reviewed_paths":["app.go"],"omitted_paths":[],"residual_risks":[]}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	report := partialReportCandidate(model.ReviewReport{}, path)
	if report == nil || len(report.Findings) != 1 || report.Findings[0].ID != "leak" {
		t.Fatalf("checkpoint candidate = %#v", report)
	}
}

func TestQuotaRetryAtUsesProviderTimezone(t *testing.T) {
	now := time.Date(2026, 8, 25, 15, 22, 0, 0, time.UTC)
	retryAt, quota := QuotaRetryAt("You've hit your session limit · resets 11:50am (America/New_York)", now)
	if !quota {
		t.Fatal("quota failure was not recognized")
	}
	want := time.Date(2026, 8, 25, 15, 50, 0, 0, time.UTC)
	if !retryAt.Equal(want) {
		t.Fatalf("retry at = %s, want %s", retryAt, want)
	}
}

func TestQuotaRetryAtParsesHourOnlyReset(t *testing.T) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 23, 30, 0, 0, eastern)
	retryAt, quota := QuotaRetryAt("You've hit your session limit; resets 4am", now)
	if !quota {
		t.Fatal("quota failure was not recognized")
	}
	want := time.Date(2026, 8, 26, 4, 0, 0, 0, eastern)
	if !retryAt.Equal(want) {
		t.Fatalf("retry at = %s, want %s", retryAt, want)
	}
}

func TestQuotaRetryAtKeepsLocalHourAcrossDSTBoundary(t *testing.T) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 23, 30, 0, 0, eastern)
	retryAt, quota := QuotaRetryAt("You've hit your session limit; resets 4am", now)
	want := time.Date(2026, 11, 1, 4, 0, 0, 0, eastern)
	if !quota || !retryAt.Equal(want) || retryAt.Hour() != 4 {
		t.Fatalf("DST reset = %s quota=%v, want %s", retryAt, quota, want)
	}
}

func TestQuotaRetryAtRejectsInvalidHourOnlyResetTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC)
	retryAt, quota := QuotaRetryAt("You've hit your session limit; resets 25am", now)
	if !quota || !retryAt.IsZero() {
		t.Fatalf("invalid reset classification = retryAt %s quota %v", retryAt, quota)
	}
}

func TestReadCodexTelemetryRecordsResolvedModelUsageAndCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	contents := `{"type":"thread.started","model_name":"gpt-5.6"}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":300,"reasoning_tokens":125}}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	telemetry, err := readCodexTelemetry(path, "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	if telemetry.Model != "gpt-5.6" || telemetry.ModelSource != "provider" {
		t.Fatalf("model telemetry = %#v", telemetry)
	}
	usage := telemetry.Usage
	if !usage.TurnsKnown || usage.Turns != 1 || usage.InputTokens != 1000 || usage.CachedInputTokens != 200 || usage.OutputTokens != 300 {
		t.Fatalf("usage telemetry = %#v", usage)
	}
	if !usage.ThinkingTokensKnown || usage.ThinkingTokens != 125 {
		t.Fatalf("thinking telemetry = %#v", usage)
	}
	if !usage.APIEquivalentCostKnown || math.Abs(usage.APIEquivalentCostUSD-0.00928) > 0.0000001 {
		t.Fatalf("API-equivalent cost = %.8f, want 0.00928", usage.APIEquivalentCostUSD)
	}
}

func TestReadCodexTelemetryRecordsReasoningOutputTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	contents := `{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":300,"reasoning_output_tokens":125}}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	telemetry, err := readCodexTelemetry(path, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if !telemetry.Usage.ThinkingTokensKnown || telemetry.Usage.ThinkingTokens != 125 {
		t.Fatalf("thinking telemetry = %#v", telemetry.Usage)
	}
}

func TestReadCodexTelemetryMarksMixedTurnThinkingPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	contents := `{"type":"turn.completed","usage":{"input_tokens":1000,"output_tokens":300,"reasoning_output_tokens":125}}
{"type":"turn.completed","usage":{"input_tokens":800,"output_tokens":200}}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	telemetry, err := readCodexTelemetry(path, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	usage := telemetry.Usage
	if usage.Turns != 2 || usage.InputTokens != 1800 || usage.OutputTokens != 500 || usage.ThinkingTokens != 125 {
		t.Fatalf("usage telemetry = %#v", usage)
	}
	if usage.ThinkingTokensKnown || !usage.ThinkingTokensPartial {
		t.Fatalf("thinking completeness = %#v", usage)
	}
}

func TestLookPathWithFallback(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	got, err := lookPathWithFallback("codex", []string{executable})
	if err != nil {
		t.Fatal(err)
	}
	if got != executable {
		t.Fatalf("resolved path = %q, want %q", got, executable)
	}
}

func TestCodexChatGPTAuthenticationCanBeReportedOnStderr(t *testing.T) {
	if !codexUsesChatGPT(nil, []byte("Logged in using ChatGPT\n")) {
		t.Fatal("expected ChatGPT authentication reported on stderr to be accepted")
	}
}

func TestPreparePrivateFileTightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.raw.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := preparePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %#o, want 0600", got)
	}
}

func TestSchemaForClaudeOmitsDialectDeclaration(t *testing.T) {
	schema, err := schemaForClaude([]byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"verdict": {"const": "approve"}}
}`))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(schema, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["$schema"]; exists {
		t.Fatalf("Claude schema still declares a dialect: %s", schema)
	}
	if _, exists := document["properties"]; !exists {
		t.Fatalf("Claude schema lost its constraints: %s", schema)
	}
}

func TestReadClaudeStructuredReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	contents := `{
  "type": "result",
  "is_error": false,
  "structured_output": {
    "schema_version": "1",
    "verdict": "approve",
    "context_complete": true,
    "summary": "Looks good",
    "findings": [],
    "reviewed_paths": ["app.go"],
    "omitted_paths": [],
    "residual_risks": []
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := readClaudeReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "approve" || !report.ContextComplete {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestReadClaudeOutputRecordsResolvedModelTurnsThinkingAndCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	contents := `{
  "type": "result",
  "is_error": false,
  "num_turns": 4,
  "total_cost_usd": 0.1234,
  "resolved_model": "claude-fable-5",
  "modelUsage": {
    "claude-fable-5": {
      "inputTokens": 1000,
      "cacheReadInputTokens": 400,
      "cacheCreationInputTokens": 100,
      "outputTokens": 200,
      "thinkingTokens": 80,
      "costUSD": 0.1234
    }
  },
  "structured_output": {
    "schema_version": "1",
    "verdict": "approve",
    "context_complete": true,
    "summary": "Looks good",
    "findings": [],
    "reviewed_paths": ["app.go"],
    "omitted_paths": [],
    "residual_risks": []
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := readClaudeOutput(path, "opus")
	if err != nil {
		t.Fatal(err)
	}
	if output.Telemetry.Model != "claude-fable-5" || output.Telemetry.ModelSource != "provider" {
		t.Fatalf("model telemetry = %#v", output.Telemetry)
	}
	usage := output.Telemetry.Usage
	if !usage.TurnsKnown || usage.Turns != 4 || usage.InputTokens != 1500 || usage.CachedInputTokens != 400 || usage.OutputTokens != 200 {
		t.Fatalf("usage telemetry = %#v", usage)
	}
	if !usage.ThinkingTokensKnown || usage.ThinkingTokens != 80 {
		t.Fatalf("thinking telemetry = %#v", usage)
	}
	if !usage.APIEquivalentCostKnown || usage.APIEquivalentCostUSD != 0.1234 {
		t.Fatalf("cost telemetry = %#v", usage)
	}
}

func TestReadClaudeOutputRecordsNestedThinkingTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	contents := `{
  "type": "result",
  "is_error": false,
  "num_turns": 2,
  "modelUsage": {
    "claude-opus-5": {
      "inputTokens": 1000,
      "outputTokens": 200,
      "output_tokens_details": {"thinking_tokens": 80}
    }
  },
  "structured_output": {
    "schema_version": "1",
    "verdict": "approve",
    "context_complete": true,
    "summary": "Looks good",
    "findings": [],
    "reviewed_paths": ["app.go"],
    "omitted_paths": [],
    "residual_risks": []
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := readClaudeOutput(path, "opus")
	if err != nil {
		t.Fatal(err)
	}
	if !output.Telemetry.Usage.ThinkingTokensKnown || output.Telemetry.Usage.ThinkingTokens != 80 {
		t.Fatalf("thinking telemetry = %#v", output.Telemetry.Usage)
	}
}

func TestReadClaudeOutputMarksMixedModelThinkingPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	contents := `{
  "type": "result",
  "is_error": false,
  "num_turns": 3,
  "modelUsage": {
    "claude-fable-5": {
      "inputTokens": 1000,
      "outputTokens": 200,
      "output_tokens_details": {"thinking_tokens": 80}
    },
    "claude-haiku-4-5": {
      "inputTokens": 100,
      "outputTokens": 20
    }
  },
  "structured_output": {
    "schema_version": "1",
    "verdict": "approve",
    "context_complete": true,
    "summary": "Looks good",
    "findings": [],
    "reviewed_paths": ["app.go"],
    "omitted_paths": [],
    "residual_risks": []
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := readClaudeOutput(path, "opus")
	if err != nil {
		t.Fatal(err)
	}
	usage := output.Telemetry.Usage
	if usage.InputTokens != 1100 || usage.OutputTokens != 220 || usage.ThinkingTokens != 80 {
		t.Fatalf("usage telemetry = %#v", usage)
	}
	if usage.ThinkingTokensKnown || !usage.ThinkingTokensPartial {
		t.Fatalf("thinking completeness = %#v", usage)
	}
}

func TestReadClaudeErrorEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(path, []byte(`{"type":"result","is_error":true,"result":null,"structured_output":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeReport(path); err == nil {
		t.Fatal("expected Claude error envelope to fail")
	}
}

func TestReadClaudeErrorEnvelopeIncludesProviderError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(path, []byte(`{"type":"result","is_error":true,"terminal_reason":"max_turns","errors":["Reached maximum number of turns (20)"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readClaudeReport(path)
	if err == nil || err.Error() != "Reached maximum number of turns (20)" {
		t.Fatalf("unexpected Claude error: %v", err)
	}
}

func TestReadClaudeErrorEnvelopeRetainsStructuredReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	contents := `{
  "type": "result",
  "is_error": true,
  "terminal_reason": "timeout",
  "errors": ["review timed out"],
  "structured_output": {
    "schema_version": "1",
    "verdict": "request_changes",
    "context_complete": false,
    "summary": "Confirmed one issue before timeout",
    "findings": [{
      "id": "leak",
      "severity": "minor",
      "confidence": 0.9,
      "file": "app.go",
      "line": 12,
      "claim": "The file remains open on an error path.",
      "evidence": "The return at app.go:12 bypasses Close.",
      "suggested_fix": "Defer Close after Open."
    }],
    "reviewed_paths": ["app.go"],
    "omitted_paths": ["auth.go"],
    "residual_risks": ["auth.go was not inspected"]
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := readClaudeOutput(path, "opus")
	if err == nil || err.Error() != "review timed out" {
		t.Fatalf("unexpected Claude error: %v", err)
	}
	if len(output.Report.Findings) != 1 || output.Report.Findings[0].ID != "leak" {
		t.Fatalf("structured partial report was discarded: %#v", output.Report)
	}
}
