package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestSupportsClaudeUltracodeVersions(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: ""},
		{version: "unknown"},
		{version: "2.1"},
		{version: "1.99.999 (Claude Code)"},
		{version: "2.0.999 (Claude Code)"},
		{version: "2.1.204 (Claude Code)"},
		{version: "2.1.205", want: true},
		{version: "2.1.205 (Claude Code)", want: true},
		{version: "2.1.282 (Claude Code)", want: true},
		{version: "2.2.0 (Claude Code)", want: true},
		{version: "3.0.0 (Claude Code)", want: true},
	} {
		t.Run(test.version, func(t *testing.T) {
			if got := supportsClaudeUltracode(test.version); got != test.want {
				t.Fatalf("supportsClaudeUltracode(%q) = %t, want %t", test.version, got, test.want)
			}
		})
	}
}

func TestClaudeUltracodeRejectsUnsupportedCLIBeforeAuthentication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, version := range []string{"2.1.204 (Claude Code)", "unknown"} {
		t.Run(version, func(t *testing.T) {
			directory := t.TempDir()
			command := filepath.Join(directory, "claude")
			script := `#!/bin/sh
printf '%s\n' "$1" >> invocations
if [ "$1" = "--version" ]; then cat version.txt; exit 0; fi
exit 91
`
			if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "version.txt"), []byte(version+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result := (Claude{Config: config.Reviewer{
				Command: command, Model: "opus", Effort: "ultracode", MaxTurns: 5, FinalizationTurns: 2,
			}}).Review(context.Background(), Request{WorkDir: directory, Timeout: 5 * time.Second})
			if result.Status != "incomplete" || result.Effort != "ultracode" || !strings.Contains(result.Error, "ultracode") || !strings.Contains(result.Error, "2.1.205") {
				t.Fatalf("unsupported CLI result = %#v", result)
			}
			invocations, err := os.ReadFile(filepath.Join(directory, "invocations"))
			if err != nil {
				t.Fatal(err)
			}
			if string(invocations) != "--version\n" {
				t.Fatalf("unsupported CLI proceeded beyond version check: %q", invocations)
			}
		})
	}
}

func TestClaudeUltracodeReviewKeepsFinalizationIsolated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, test := range []struct {
		reviewer string
		cause    string
	}{
		{reviewer: "claude-security", cause: "security_sensitive"},
		{reviewer: "claude-cross-examination", cause: "blocking_cross_examination"},
	} {
		t.Run(test.reviewer, func(t *testing.T) {
			directory := t.TempDir()
			command := filepath.Join(directory, "claude")
			script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2.1.282 (Claude Code)'; exit 0; fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  echo '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}'
  exit 0
fi
if [ ! -f inspection.args ]; then
  printf '%s\000' "$@" > inspection.args
  cat > inspection.prompt
  echo '{"type":"result","is_error":true,"terminal_reason":"max_turns","errors":["Reached maximum number of turns (3)"],"num_turns":3,"total_cost_usd":1,"usage":{"input_tokens":100,"output_tokens":20,"thinking_tokens":10},"structured_output":{"schema_version":"1","verdict":"abstain","context_complete":false,"summary":"inspection was incomplete","findings":[],"reviewed_paths":[],"omitted_paths":["app.go"],"residual_risks":["turn ceiling reached"]}}'
  exit 1
fi
printf '%s\000' "$@" > finalization.args
cat > finalization.prompt
echo '{"type":"result","is_error":false,"num_turns":1,"total_cost_usd":0.5,"usage":{"input_tokens":25,"output_tokens":10,"thinking_tokens":2},"structured_output":{"schema_version":"1","verdict":"abstain","context_complete":false,"summary":"inspection was incomplete","findings":[],"reviewed_paths":[],"omitted_paths":["app.go"],"residual_risks":["turn ceiling reached"]}}'
`
			if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			request := Request{
				WorkDir: directory, RuntimeDir: filepath.Join(directory, "runtime"),
				RecoveryDir: filepath.Join(directory, "recovery"), RunDir: filepath.Join(directory, "run"),
				Target: model.Target{BaseSHA: "base", HeadSHA: "head"}, Schema: []byte(`{"type":"object"}`),
				Prompt: "review app.go", Policy: "trusted review policy", Timeout: 5 * time.Second,
				ChangedPaths: []string{"app.go"},
			}
			for _, path := range []string{request.RunDir, request.RuntimeDir, request.RecoveryDir} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Reviewer{
				Command: command, Model: "fable", Effort: "ultracode", MaxTurns: 5, FinalizationTurns: 2, MaxBudgetUSD: 5,
			}
			result := (Claude{Config: cfg, ReviewerName: test.reviewer, EscalationCause: test.cause}).Review(context.Background(), request)
			if result.Status != "completed" || result.Report == nil || result.Report.Verdict != "abstain" || result.Report.ContextComplete {
				t.Fatalf("reserved-turn result = %#v", result)
			}
			if result.Effort != "ultracode" || result.Reviewer != test.reviewer || result.EscalationCause != test.cause || result.Report.Reviewer != test.reviewer {
				t.Fatalf("configured review metadata was not preserved: %#v", result)
			}
			if result.Usage.Turns != 4 || !result.Usage.TurnsKnown || result.Usage.APIEquivalentCostUSD != 1.5 || !result.Usage.APIEquivalentCostKnown {
				t.Fatalf("reserved-turn usage = %#v", result.Usage)
			}

			baselineConfig := cfg
			baselineConfig.Effort = "high"
			baselineArgs := claudeReviewArgs(baselineConfig, request, request.Schema, "", 3, claudeInspectionTools("high"))
			var baselineSettings map[string]any
			if err := json.Unmarshal([]byte(valueAfter(baselineArgs, "--settings")), &baselineSettings); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []struct {
				name, effort, tools, turns, budget string
				workflow                           bool
			}{
				{name: "inspection", effort: "ultracode", tools: "Read,Glob,Grep,Bash,Agent,Workflow,TaskStop", turns: "3", budget: "5", workflow: true},
				{name: "finalization", effort: "xhigh", tools: "", turns: "2", budget: "4"},
			} {
				contents, err := os.ReadFile(filepath.Join(directory, phase.name+".args"))
				if err != nil {
					t.Fatal(err)
				}
				args := strings.Split(strings.TrimSuffix(string(contents), "\x00"), "\x00")
				for flag, want := range map[string]string{
					"--effort": phase.effort, "--tools": phase.tools, "--max-turns": phase.turns,
					"--max-budget-usd": phase.budget, "--permission-mode": "dontAsk", "--model": "fable",
					"--append-system-prompt": request.Policy,
				} {
					if !slices.Contains(args, flag) || valueAfter(args, flag) != want {
						t.Errorf("%s %s = %q, want %q; args = %q", phase.name, flag, valueAfter(args, flag), want, args)
					}
				}
				for _, flag := range []string{"-p", "--safe-mode", "--no-session-persistence"} {
					if !slices.Contains(args, flag) {
						t.Errorf("%s lacks %s", phase.name, flag)
					}
				}
				if phase.workflow {
					if valueAfter(args, "--allowedTools") != "Workflow" || valueAfter(args, "--append-subagent-system-prompt") != request.Policy {
						t.Errorf("inspection lacks workflow authorization or delegated policy: %q", args)
					}
				} else if slices.Contains(args, "--allowedTools") || slices.Contains(args, "--append-subagent-system-prompt") {
					t.Errorf("finalizer retained delegation flags: %q", args)
				}
				var settings map[string]any
				if err := json.Unmarshal([]byte(valueAfter(args, "--settings")), &settings); err != nil {
					t.Fatal(err)
				}
				if phase.workflow && settings["enableWorkflows"] != true {
					t.Errorf("inspection did not enable workflows: %#v", settings)
				}
				if !phase.workflow {
					if _, exists := settings["enableWorkflows"]; exists {
						t.Errorf("finalizer retained workflow setting: %#v", settings)
					}
				}
				delete(settings, "enableWorkflows")
				if !reflect.DeepEqual(settings, baselineSettings) {
					t.Errorf("%s altered ordinary review sandbox: got %#v, want %#v", phase.name, settings, baselineSettings)
				}
			}
			for _, name := range []string{test.reviewer + ".inspection.raw.json", test.reviewer + ".raw.json", test.reviewer + "-finalization.effective-prompt.md"} {
				if _, err := os.Stat(filepath.Join(request.RunDir, name)); err != nil {
					t.Errorf("missing %s: %v", name, err)
				}
			}
			finalPrompt, err := os.ReadFile(filepath.Join(directory, "finalization.prompt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(finalPrompt), "tools are mechanically disabled") {
				t.Fatalf("finalizer did not receive serialization-only policy: %s", finalPrompt)
			}
		})
	}
}
