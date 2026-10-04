package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/herikwebb/cora/internal/model"
)

type geminiOutput struct {
	Report    model.ReviewReport
	Telemetry reviewerTelemetry
}

func readGeminiOutput(path, fallbackModel string) (geminiOutput, error) {
	output := geminiOutput{Telemetry: reviewerTelemetry{Model: fallbackModel}}
	if fallbackModel != "" {
		output.Telemetry.ModelSource = "configured"
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return output, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(contents, &envelope); err != nil || envelope == nil {
		return output, errors.New("Gemini output is not a JSON result envelope")
	}
	output.Telemetry = geminiTelemetry(envelope["stats"], fallbackModel)
	var response string
	responseErr := json.Unmarshal(envelope["response"], &response)
	if responseErr == nil {
		output.Report, responseErr = parseGeminiReportText(response)
	}
	if raw := bytes.TrimSpace(envelope["error"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var detail struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		_ = json.Unmarshal(raw, &detail)
		if detail.Type != "" && detail.Message != "" {
			return output, fmt.Errorf("Gemini returned an error: %s: %s", detail.Type, detail.Message)
		}
		return output, fmt.Errorf("Gemini returned an error: %s", firstNonEmpty(detail.Message, detail.Type, string(raw)))
	}
	if raw := bytes.TrimSpace(envelope["warnings"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var warnings []string
		if err := json.Unmarshal(raw, &warnings); err != nil {
			return output, fmt.Errorf("Gemini result contained invalid warnings: %w", err)
		}
		for _, warning := range warnings {
			message := strings.ToLower(warning)
			if strings.Contains(message, "maximum session turns exceeded") || strings.Contains(message, "loop detected") || strings.Contains(message, "agent execution stopped") {
				return output, fmt.Errorf("Gemini review stopped: %s", warning)
			}
		}
	}
	if responseErr != nil {
		return output, fmt.Errorf("Gemini result did not contain a valid structured report: %w", responseErr)
	}
	return output, nil
}

// Gemini CLI does not enforce a response schema. Validate its full wire
// representation before decoding it into structs whose zero values could hide
// missing or null required fields.
func parseGeminiReportText(value string) (model.ReviewReport, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") {
		lines := strings.Split(value, "\n")
		if len(lines) < 3 || strings.TrimSpace(lines[0]) != "```json" || strings.TrimSpace(lines[len(lines)-1]) != "```" {
			return model.ReviewReport{}, errors.New("Gemini report must contain only JSON or one json code fence")
		}
		value = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	contents := []byte(value)
	fields, err := geminiReportObject(contents, "report", []string{
		"schema_version", "verdict", "context_complete", "summary", "findings", "reviewed_paths", "omitted_paths", "residual_risks",
	}, nil)
	if err != nil {
		return model.ReviewReport{}, err
	}
	for _, field := range []string{"reviewed_paths", "omitted_paths", "residual_risks"} {
		if err := geminiStringArray(fields[field], field); err != nil {
			return model.ReviewReport{}, err
		}
	}
	var findings []json.RawMessage
	if err := json.Unmarshal(fields["findings"], &findings); err != nil {
		return model.ReviewReport{}, fmt.Errorf("findings must be an array: %w", err)
	}
	for index, raw := range findings {
		location := fmt.Sprintf("findings[%d]", index)
		finding, err := geminiReportObject(raw, location, []string{
			"id", "severity", "confidence", "file", "line", "claim", "evidence", "suggested_fix", "disposition", "reachability",
		}, []string{"disposition", "reachability"})
		if err != nil {
			return model.ReviewReport{}, err
		}
		if !bytes.Equal(bytes.TrimSpace(finding["disposition"]), []byte("null")) {
			var disposition string
			if err := json.Unmarshal(finding["disposition"], &disposition); err != nil || disposition == "" {
				return model.ReviewReport{}, fmt.Errorf("%s.disposition must be a valid disposition or null", location)
			}
		}
		if !bytes.Equal(bytes.TrimSpace(finding["reachability"]), []byte("null")) {
			reachability, err := geminiReportObject(finding["reachability"], location+".reachability", []string{
				"status", "trigger", "path", "impact", "preconditions",
			}, nil)
			if err != nil {
				return model.ReviewReport{}, err
			}
			for _, field := range []string{"path", "preconditions"} {
				if err := geminiStringArray(reachability[field], location+".reachability."+field); err != nil {
					return model.ReviewReport{}, err
				}
			}
		}
	}
	var report model.ReviewReport
	if err := json.Unmarshal(contents, &report); err != nil {
		return model.ReviewReport{}, err
	}
	for index, finding := range report.Findings {
		if finding.Line < 0 {
			return model.ReviewReport{}, fmt.Errorf("finding %d has a negative line", index)
		}
	}
	if err := validateReport(report); err != nil {
		return model.ReviewReport{}, err
	}
	return report, nil
}

func geminiReportObject(contents []byte, location string, required, nullable []string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(required))
	for _, field := range required {
		allowed[field] = true
	}
	allowsNull := make(map[string]bool, len(nullable))
	for _, field := range nullable {
		allowsNull[field] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%s must be an object", location)
	}
	fields := make(map[string]json.RawMessage, len(required))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		field, ok := token.(string)
		if !ok || !allowed[field] {
			return nil, fmt.Errorf("%s contains unknown field %q", location, field)
		}
		if _, found := fields[field]; found {
			return nil, fmt.Errorf("%s contains duplicate field %q", location, field)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if !allowsNull[field] && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("%s.%s cannot be null", location, field)
		}
		fields[field] = raw
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("%s has an invalid object boundary", location)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s has trailing content", location)
	}
	for _, field := range required {
		if _, found := fields[field]; !found {
			return nil, fmt.Errorf("%s is missing required field %q", location, field)
		}
	}
	return fields, nil
}

func geminiStringArray(contents []byte, location string) error {
	var values []json.RawMessage
	if err := json.Unmarshal(contents, &values); err != nil || values == nil {
		return fmt.Errorf("%s must be an array of strings", location)
	}
	for _, value := range values {
		var item string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &item) != nil {
			return fmt.Errorf("%s must contain only strings", location)
		}
	}
	return nil
}

func geminiTelemetry(contents []byte, fallbackModel string) reviewerTelemetry {
	telemetry := reviewerTelemetry{Model: fallbackModel}
	if fallbackModel != "" {
		telemetry.ModelSource = "configured"
	}
	var stats struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(contents, &stats) != nil || len(stats.Models) == 0 {
		return telemetry
	}
	names := make([]string, 0, len(stats.Models))
	for name := range stats.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	var thinking geminiCounterSum
	var input, cached, output geminiCounterSum
	var bestModel string
	var bestRequests int64 = -1
	var bestMainRequests int64 = -1
	bestMain := false
	for _, name := range names {
		var entry struct {
			API    map[string]json.RawMessage `json:"api"`
			Tokens map[string]json.RawMessage `json:"tokens"`
			Roles  map[string]json.RawMessage `json:"roles"`
		}
		if json.Unmarshal(stats.Models[name], &entry) != nil {
			thinking.add(0, false)
			continue
		}
		requests, known := geminiCounter(entry.API, "totalRequests")
		thoughts, thoughtsKnown := geminiCounter(entry.Tokens, "thoughts")
		thinking.add(thoughts, thoughtsKnown)
		prompt, promptKnown := geminiCounter(entry.Tokens, "prompt", "input")
		input.add(prompt, promptKnown)
		cache, cacheKnown := geminiCounter(entry.Tokens, "cached")
		cached.add(cache, cacheKnown)
		candidates, candidatesKnown := geminiCounter(entry.Tokens, "candidates")
		output.add(candidates, candidatesKnown)
		var mainRole map[string]json.RawMessage
		_ = json.Unmarshal(entry.Roles["main"], &mainRole)
		mainRequests, main := geminiCounter(mainRole, "totalRequests")
		better := main && !bestMain
		if main == bestMain {
			if main && mainRequests != bestMainRequests {
				better = mainRequests > bestMainRequests
			} else {
				better = known && requests > bestRequests
			}
		}
		if strings.TrimSpace(name) != "" && better {
			bestModel, bestRequests, bestMain = name, requests, main
			bestMainRequests = mainRequests
		}
	}
	if bestModel != "" {
		telemetry.Model = bestModel
		telemetry.ModelSource = "provider"
	}
	// API request counts include retries and utility calls, so they do not
	// establish the session turns needed to enforce an auto-fix turn ceiling.
	telemetry.Usage = model.Usage{
		InputTokens: input.total, CachedInputTokens: cached.total, OutputTokens: output.total,
		ThinkingTokens: thinking.total, ThinkingTokensKnown: thinking.seen && !thinking.invalid, ThinkingTokensPartial: thinking.seen && thinking.invalid,
	}
	return telemetry
}

func geminiCounter(values map[string]json.RawMessage, names ...string) (int64, bool) {
	for _, name := range names {
		raw, found := values[name]
		if !found {
			continue
		}
		var value int64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 0 {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

type geminiCounterSum struct {
	total   int64
	seen    bool
	invalid bool
}

func (sum *geminiCounterSum) add(value int64, known bool) {
	if !known {
		sum.invalid = true
		return
	}
	sum.seen = true
	if value > math.MaxInt64-sum.total {
		sum.invalid = true
		return
	}
	sum.total += value
}
