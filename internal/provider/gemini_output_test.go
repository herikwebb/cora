package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const geminiValidReport = `{"schema_version":"1","verdict":"approve","context_complete":true,"summary":"Reviewed the change.","findings":[],"reviewed_paths":["main.go"],"omitted_paths":[],"residual_risks":[]}`
const geminiValidFinding = `{"id":"F1","severity":"minor","confidence":0.8,"file":"main.go","line":3,"claim":"An edge case fails.","evidence":"The branch skips the guard.","suggested_fix":"Check the guard.","disposition":null,"reachability":null}`

func TestParseGeminiReportTextAcceptsCanonicalReportAndJSONFence(t *testing.T) {
	plain, err := parseGeminiReportText(geminiValidReport)
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := parseGeminiReportText(" \n```json\n" + geminiValidReport + "\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, fenced) || plain.Verdict != "approve" || !plain.ContextComplete {
		t.Fatalf("fenced report differs: %#v versus %#v", plain, fenced)
	}
	withFinding := strings.Replace(geminiValidReport, `"findings":[]`, `"findings":[`+geminiValidFinding+`]`, 1)
	if report, err := parseGeminiReportText(withFinding); err != nil || len(report.Findings) != 1 {
		t.Fatalf("valid nullable finding = %#v, %v", report, err)
	}
}

func TestParseGeminiReportTextRequiresEveryTopLevelField(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(geminiValidReport), &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		t.Run(field, func(t *testing.T) {
			copy := make(map[string]json.RawMessage, len(fields))
			for name, value := range fields {
				copy[name] = value
			}
			delete(copy, field)
			contents, err := json.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseGeminiReportText(string(contents)); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("missing %s error = %v", field, err)
			}
		})
	}
}

func TestParseGeminiReportTextRejectsSchemaViolations(t *testing.T) {
	withFinding := strings.Replace(geminiValidReport, `"findings":[]`, `"findings":[`+geminiValidFinding+`]`, 1)
	reachability := `{"status":"not_applicable","trigger":"","path":[],"impact":"","preconditions":[]}`
	withReachability := strings.Replace(withFinding, `"reachability":null`, `"reachability":`+reachability, 1)
	tests := map[string]string{
		"unknown field":          strings.Replace(geminiValidReport, `"summary":`, `"extra":false,"summary":`, 1),
		"duplicate field":        strings.Replace(geminiValidReport, `"verdict":`, `"verdict":"abstain","verdict":`, 1),
		"null boolean":           strings.Replace(geminiValidReport, `"context_complete":true`, `"context_complete":null`, 1),
		"null summary":           strings.Replace(geminiValidReport, `"summary":"Reviewed the change."`, `"summary":null`, 1),
		"wrong boolean type":     strings.Replace(geminiValidReport, `"context_complete":true`, `"context_complete":"true"`, 1),
		"null findings":          strings.Replace(geminiValidReport, `"findings":[]`, `"findings":null`, 1),
		"null array element":     strings.Replace(geminiValidReport, `["main.go"]`, `[null]`, 1),
		"nonarray findings":      strings.Replace(geminiValidReport, `"findings":[]`, `"findings":{}`, 1),
		"null finding":           strings.Replace(geminiValidReport, `"findings":[]`, `"findings":[null]`, 1),
		"missing nullable field": strings.Replace(withFinding, `,"disposition":null`, "", 1),
		"empty disposition":      strings.Replace(withFinding, `"disposition":null`, `"disposition":""`, 1),
		"bad disposition":        strings.Replace(withFinding, `"disposition":null`, `"disposition":"ignored"`, 1),
		"negative line":          strings.Replace(withFinding, `"line":3`, `"line":-1`, 1),
		"fractional line":        strings.Replace(withFinding, `"line":3`, `"line":1.5`, 1),
		"null required finding":  strings.Replace(withFinding, `"file":"main.go"`, `"file":null`, 1),
		"missing reachability":   strings.Replace(withReachability, `,"preconditions":[]`, "", 1),
		"null reachability path": strings.Replace(withReachability, `"path":[]`, `"path":null`, 1),
		"null path element":      strings.Replace(withReachability, `"path":[]`, `"path":[null]`, 1),
		"unknown reachability":   strings.Replace(withReachability, `"trigger":""`, `"trigger":"","extra":1`, 1),
		"major lacks proof":      strings.Replace(withFinding, `"severity":"minor"`, `"severity":"major"`, 1),
		"bad confidence":         strings.Replace(withFinding, `"confidence":0.8`, `"confidence":1.1`, 1),
		"invalid verdict":        strings.Replace(geminiValidReport, `"approve"`, `"approved"`, 1),
		"wrong schema":           strings.Replace(geminiValidReport, `"schema_version":"1"`, `"schema_version":"2"`, 1),
		"prose prefix":           "Review complete:\n" + geminiValidReport,
		"prose suffix":           geminiValidReport + "\nDone.",
		"multiple reports":       geminiValidReport + "\n" + geminiValidReport,
		"wrong fence language":   "```text\n" + geminiValidReport + "\n```",
		"untyped fence":          "```\n" + geminiValidReport + "\n```",
		"fence with prose":       "```json\n" + geminiValidReport + "\n```\nDone.",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGeminiReportText(contents); err == nil {
				t.Fatal("invalid report was accepted")
			}
		})
	}
}

func TestReadGeminiOutputAggregatesTelemetryAndPrefersMainRole(t *testing.T) {
	path := writeGeminiOutput(t, map[string]any{
		"response": geminiValidReport,
		"stats": json.RawMessage(`{"models":{
			"gemini-flash":{"api":{"totalRequests":10},"tokens":{"prompt":100,"cached":25,"candidates":20,"thoughts":5}},
			"gemini-pro":{"api":{"totalRequests":2},"tokens":{"prompt":200,"cached":50,"candidates":30,"thoughts":15},"roles":{"main":{"totalRequests":2,"tokens":{"prompt":200,"thoughts":15}}}}
		}}`),
	})
	output, err := readGeminiOutput(path, "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	usage := output.Telemetry.Usage
	if output.Telemetry.Model != "gemini-pro" || output.Telemetry.ModelSource != "provider" || usage.InputTokens != 300 || usage.CachedInputTokens != 75 || usage.OutputTokens != 50 || usage.ThinkingTokens != 20 || !usage.ThinkingTokensKnown {
		t.Fatalf("Gemini telemetry = %#v", output.Telemetry)
	}
	if usage.TurnsKnown || usage.Turns != 0 || usage.APIEquivalentCostKnown || usage.APIEquivalentCostUSD != 0 || usage.CostSource != "" {
		t.Fatalf("Gemini telemetry fabricated turn or cost usage: %#v", usage)
	}
}

func TestGeminiTelemetrySelectsMostUsedModelWithoutMainRole(t *testing.T) {
	telemetry := geminiTelemetry([]byte(`{"models":{"a":{"api":{"totalRequests":1}},"b":{"api":{"totalRequests":3},"tokens":{"input":25}}}}`), "configured")
	if telemetry.Model != "b" || telemetry.ModelSource != "provider" || telemetry.Usage.InputTokens != 25 || telemetry.Usage.ThinkingTokensKnown {
		t.Fatalf("model telemetry = %#v", telemetry)
	}
}

func TestGeminiTelemetryKeepsMissingAndInvalidCountersUnknown(t *testing.T) {
	tests := map[string]string{
		"missing stats":       "",
		"null stats":          "null",
		"malformed stats":     `{"models":`,
		"empty models":        `{"models":{}}`,
		"missing counters":    `{"models":{"model":{}}}`,
		"negative counters":   `{"models":{"model":{"api":{"totalRequests":-2},"tokens":{"prompt":-1,"thoughts":-3}}}}`,
		"null counters":       `{"models":{"model":{"api":{"totalRequests":null},"tokens":{"thoughts":null}}}}`,
		"noninteger counters": `{"models":{"model":{"api":{"totalRequests":1.5},"tokens":{"thoughts":"5"}}}}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			telemetry := geminiTelemetry([]byte(contents), "configured")
			if telemetry.Usage.TurnsKnown || telemetry.Usage.ThinkingTokensKnown || telemetry.Usage.APIEquivalentCostKnown || telemetry.Usage.InputTokens != 0 || telemetry.Usage.ThinkingTokens != 0 {
				t.Fatalf("unknown telemetry became known: %#v", telemetry)
			}
		})
	}
	partial := geminiTelemetry([]byte(`{"models":{"a":{"tokens":{"thoughts":5}},"b":{"tokens":{"thoughts":-1}}}}`), "configured")
	if partial.Usage.ThinkingTokensKnown || !partial.Usage.ThinkingTokensPartial || partial.Usage.ThinkingTokens != 5 {
		t.Fatalf("partial thoughts = %#v", partial.Usage)
	}
}

func TestReadGeminiOutputPreservesReportAndTelemetryOnError(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{"provider error", "error", map[string]any{"type": "quota", "message": "Quota exhausted"}},
		{"empty error object", "error", map[string]any{}},
		{"turn limit", "warnings", []string{"Maximum session turns exceeded"}},
		{"loop detected", "warnings", []string{"Loop detected, stopping execution"}},
		{"agent stopped", "warnings", []string{"Agent execution stopped: a hook requested termination"}},
		{"escaped warning", "warnings", json.RawMessage(`["Loop\u0020detected, stopping execution"]`)},
		{"terminal warning after benign warning", "warnings", []string{"An optional capability is unavailable", "Agent execution stopped: review interrupted"}},
		{"malformed warnings", "warnings", map[string]string{"warning": "Loop detected, stopping execution"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := map[string]any{
				"response": geminiValidReport,
				"stats":    json.RawMessage(`{"models":{"gemini-pro":{"api":{"totalRequests":1},"tokens":{"prompt":20,"thoughts":3}}}}`),
				test.field: test.value,
			}
			output, err := readGeminiOutput(writeGeminiOutput(t, envelope), "fallback")
			if err == nil || output.Report.Verdict != "approve" || output.Telemetry.Model != "gemini-pro" || output.Telemetry.Usage.InputTokens != 20 {
				t.Fatalf("error result = %#v, %v", output, err)
			}
		})
	}
}

func TestReadGeminiOutputAllowsNonTerminalWarnings(t *testing.T) {
	for _, warnings := range []any{nil, []string{}, []string{"An optional capability is unavailable"}} {
		output, err := readGeminiOutput(writeGeminiOutput(t, map[string]any{
			"response": geminiValidReport,
			"warnings": warnings,
			"error":    nil,
		}), "gemini-pro")
		if err != nil || output.Report.Verdict != "approve" {
			t.Fatalf("non-terminal warnings failed review: %#v, %v", warnings, err)
		}
	}
}

func TestReadGeminiOutputRejectsMissingOrMalformedResponse(t *testing.T) {
	for _, response := range []any{nil, map[string]any{"verdict": "approve"}, "", "{}", "not JSON"} {
		path := writeGeminiOutput(t, map[string]any{"response": response})
		if _, err := readGeminiOutput(path, "gemini-pro"); err == nil {
			t.Fatalf("invalid response accepted: %#v", response)
		}
	}
	if _, err := readGeminiOutput(writeGeminiOutput(t, map[string]any{}), "gemini-pro"); err == nil {
		t.Fatal("missing response accepted")
	}
}

func writeGeminiOutput(t *testing.T, envelope map[string]any) string {
	t.Helper()
	contents, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gemini-output.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
