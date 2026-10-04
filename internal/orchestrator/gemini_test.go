//go:build !windows

package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/model"
)

func TestRunnerThreeReviewerConsensusIncludesGeminiAndCanRetryIt(t *testing.T) {
	repo, target := fableRetryTestTarget(t)
	configureGeminiAuth(t)
	binDir := t.TempDir()
	codexPath, claudePath, geminiPath := filepath.Join(binDir, "codex"), filepath.Join(binDir, "claude"), filepath.Join(binDir, "gemini")
	writeExecutable(t, codexPath, fakeCodexScript)
	writeExecutable(t, claudePath, fakeDisputeClaudeScript)
	writeExecutable(t, geminiPath, fakeGeminiReviewScript(t, validReport))
	cfg := fableRetryConfig(codexPath, claudePath)
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.Command = geminiPath
	cfg.Reviewers.Gemini.Model = "gemini-2.5-pro"
	cfg.MinimumApprovals = 3
	cfg.Escalation.Enabled = false
	cfg.CrossExamineBlockingFindings = false

	decision, err := (Runner{Version: "test"}).Run(context.Background(), repo, target, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != model.StateApproved || len(decision.Reviewers) != 3 || decision.Reviewers["gemini"] != "approve" {
		t.Fatalf("three reviewer consensus = %#v", decision)
	}
	parent := latestFableRetryManifest(t, repo)
	gemini := reviewerResultByName(t, parent.Reviewers, "gemini")
	if gemini.Model != "gemini-2.5-pro" || gemini.Attempt != 1 || parent.ReviewPolicy == nil || !parent.ReviewPolicy.Gemini.Enabled {
		t.Fatalf("Gemini manifest metadata = %#v, policy = %#v", gemini, parent.ReviewPolicy)
	}
	if parent.Security.ReviewerIsolation != "per-reviewer-disposable-clone-provider-tool-restrictions" {
		t.Fatalf("Gemini isolation overstates sandbox = %q", parent.Security.ReviewerIsolation)
	}
	for _, name := range []string{"gemini.json", "gemini.raw.json"} {
		if _, err := os.Stat(filepath.Join(decision.RecordPath, name)); err != nil {
			t.Fatalf("missing Gemini artifact %s: %v", name, err)
		}
	}

	// Only Gemini should execute; its ordinary peers are retained as evidence.
	writeExecutable(t, codexPath, "#!/bin/sh\nexit 97\n")
	writeExecutable(t, claudePath, "#!/bin/sh\nexit 97\n")
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	limits["gemini"] = model.ReviewerExecutionLimit{Timeout: model.NewDuration(5 * time.Second), MaxTurns: 1}
	retry, err := (Runner{Version: "test"}).RunWithOptions(context.Background(), repo, target, cfg, RunOptions{
		ParentRunID: parent.RunID, RetryReviewers: map[string]bool{"gemini": true},
		ReuseReviewers: parent.Reviewers, ReviewerExecutionLimits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.State != model.StateApproved || len(retry.Reviewers) != 3 {
		t.Fatalf("Gemini targeted retry = %#v", retry)
	}
	child := latestFableRetryManifest(t, repo)
	if got := reviewerResultByName(t, child.Reviewers, "gemini"); got.Attempt != 2 || got.ReusedFromRunID != "" {
		t.Fatalf("Gemini retry metadata = %#v", got)
	}
	for _, reviewer := range []string{"codex", "claude"} {
		if got := reviewerResultByName(t, child.Reviewers, reviewer); got.ReusedFromRunID != parent.RunID {
			t.Fatalf("ordinary peer %s was not reused: %#v", reviewer, got)
		}
	}
}

func TestGeminiRetryInvalidatesDependentClaudeReviews(t *testing.T) {
	for _, role := range []string{"claude-escalation", "claude-cross-examination"} {
		t.Run(role, func(t *testing.T) {
			repo, target := fableRetryTestTarget(t)
			configureGeminiAuth(t)
			binDir := t.TempDir()
			geminiPath, claudePath := filepath.Join(binDir, "gemini"), filepath.Join(binDir, "claude")
			marker := filepath.Join(binDir, "claude-invoked")
			disputed := fableRetryRequestChanges()
			disputed.OmittedPaths = []string{}
			disputed.ResidualRisks = []string{}
			report, err := json.Marshal(disputed)
			if err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, geminiPath, fakeGeminiReviewScript(t, string(report)))
			writeExecutable(t, claudePath, failIfFableRunsScript(marker))
			cfg := fableRetryConfig("must-not-run-codex", claudePath)
			cfg.Reviewers.Gemini.Enabled = true
			cfg.Reviewers.Gemini.Command = geminiPath
			cfg.Reviewers.Gemini.Model = "gemini-2.5-pro"
			cfg.Escalation.AdjudicateDisagreements = role == "claude-escalation"
			cfg.CrossExamineBlockingFindings = role == "claude-cross-examination"
			approve := fableRetryApproval()
			options := RunOptions{
				RetryReviewers: map[string]bool{"gemini": true},
				ReuseReviewers: []model.ReviewerResult{
					{Reviewer: "codex", Status: "completed", Report: approve},
					{Reviewer: "claude", Status: "completed", Report: approve},
				},
			}
			if role == "claude-escalation" {
				options.ReuseReviewers = append(options.ReuseReviewers, model.ReviewerResult{Reviewer: role, Status: "completed", Report: approve, EscalationCause: "disputed"})
			} else {
				options.ReuseCrossExaminations = []model.ReviewerResult{{Reviewer: role, Status: "completed", Report: approve, EscalationCause: "blocking_cross_examination"}}
			}
			decision, err := (Runner{Version: "test"}).RunWithOptions(context.Background(), repo, target, cfg, options)
			if err != nil {
				t.Fatal(err)
			}
			if decision.State != model.StateIncomplete {
				t.Fatalf("Gemini upstream retry = %#v", decision)
			}
			manifest := latestFableRetryManifest(t, repo)
			results := append(manifest.Reviewers, manifest.CrossExaminations...)
			dependent := reviewerResultByName(t, results, role)
			if dependent.Status != "deferred" || dependent.FailureKind != "dependency_changed" || !dependent.Retryable {
				t.Fatalf("dependent review was reused after Gemini changed: %#v", dependent)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("unselected dependent review executed: %v", err)
			}
		})
	}
}

func TestGeminiLimitsUseIndependentTurnsAndCapacity(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reviewers.Gemini.Enabled = true
	cfg.Reviewers.Gemini.MaxConcurrency = 4
	if providerConcurrency(cfg, "gemini") != 4 {
		t.Fatal("Gemini concurrency did not use provider settings")
	}
	_, err := resolveReviewerExecutionLimits(cfg, map[string]model.ReviewerExecutionLimit{
		"gemini": {Timeout: model.NewDuration(time.Minute), MaxTurns: 1},
	})
	if err != nil {
		t.Fatalf("Gemini incorrectly used Claude finalization reserve: %v", err)
	}
	_, err = resolveReviewerExecutionLimits(cfg, map[string]model.ReviewerExecutionLimit{
		"gemini": {Timeout: model.NewDuration(time.Minute), MaxTurns: 0},
	})
	if err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("zero Gemini max turns error = %v", err)
	}
	cfg.Reviewers.Gemini = config.Reviewer{}
	if _, err := resolveReviewerExecutionLimits(cfg, config.SnapshotReviewerExecutionLimits(cfg)); err != nil {
		t.Fatalf("historical policy without Gemini is not replayable: %v", err)
	}
}

func TestChangedControlFilesRecognizesGeminiConfiguration(t *testing.T) {
	want := []string{".gemini/settings.json", "GEMINI.md", "nested/.gemini/policies/review.toml", "nested/GEMINI.md"}
	paths := append(append([]string(nil), want...), "src/gemini.go")
	if got := changedControlFiles(paths); !reflect.DeepEqual(got, want) {
		t.Fatalf("Gemini control paths = %v, want %v", got, want)
	}
}

func configureGeminiAuth(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "oauth_creds.json"), []byte(`{"refresh_token":"fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeGeminiReviewScript(t *testing.T, report string) string {
	t.Helper()
	envelope, err := json.Marshal(map[string]any{
		"response": report,
		"stats": map[string]any{"models": map[string]any{"gemini-2.5-pro": map[string]any{
			"api": map[string]int{"totalRequests": 1}, "tokens": map[string]int{"prompt": 100, "candidates": 10, "thoughts": 3},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '0.46.0'; exit 0; fi\ncat >/dev/null\ncat <<'CORA_GEMINI_JSON'\n" + string(envelope) + "\nCORA_GEMINI_JSON\n"
}
