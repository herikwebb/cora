package provider

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestCodexReviewArgsUseGenericExecForAuditedPrompt(t *testing.T) {
	request := Request{
		RepoRoot:   "/tmp/repo",
		WorkDir:    "/tmp/cora-reviewer",
		Target:     model.Target{Mode: "branch", BaseSHA: "base-sha"},
		SchemaPath: "/tmp/schema.json",
		Policy:     "trusted policy",
	}
	got := codexReviewArgs(config.Reviewer{Model: "gpt-5.6", Effort: "high"}, request, "/tmp/result.json")
	want := []string{
		"exec", "--sandbox", "read-only",
		"--cd", "/tmp/cora-reviewer",
		"--add-dir", "/tmp/repo",
		"--skip-git-repo-check", "--ignore-rules",
		"--ephemeral", "--ignore-user-config",
		"--config", `developer_instructions="trusted policy"`,
		"--model", "gpt-5.6",
		"--config", `model_reasoning_effort="high"`,
		"--output-schema", "/tmp/schema.json",
		"--json", "--output-last-message", "/tmp/result.json", "-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex args = %v, want %v", got, want)
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
