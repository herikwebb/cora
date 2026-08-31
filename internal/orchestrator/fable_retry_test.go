//go:build !windows

package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/record"
)

func TestUpstreamRetryDefersInvalidatedDisputeAdjudication(t *testing.T) {
	repo, target := fableRetryTestTarget(t)
	binDir := t.TempDir()
	codexPath := filepath.Join(binDir, "codex")
	claudePath := filepath.Join(binDir, "claude")
	marker := filepath.Join(binDir, "claude-invoked")
	writeExecutable(t, codexPath, fakeDisputingCodexScript)
	writeExecutable(t, claudePath, failIfFableRunsScript(marker))

	cfg := fableRetryConfig(codexPath, claudePath)
	cfg.Escalation.AdjudicateDisagreements = true
	cfg.CrossExamineBlockingFindings = false
	approve := fableRetryApproval()
	var progress bytes.Buffer
	decision, err := (Runner{Version: "test", Progress: &progress}).RunWithOptions(context.Background(), repo, target, cfg, RunOptions{
		RetryReviewers: map[string]bool{"codex": true},
		ReuseReviewers: []model.ReviewerResult{
			{Reviewer: "claude", Status: "completed", Report: approve},
			{Reviewer: "claude-escalation", Status: "completed", Report: approve, EscalationCause: "disputed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != model.StateIncomplete || decision.Reviewers["claude-escalation"] != "incomplete" {
		t.Fatalf("upstream retry decision = %#v", decision)
	}
	manifest := latestFableRetryManifest(t, repo)
	escalation := reviewerResultByName(t, manifest.Reviewers, "claude-escalation")
	if escalation.Status != "deferred" || escalation.FailureKind != "dependency_changed" || !escalation.Retryable || !strings.Contains(escalation.Error, "fresh targeted retry") {
		t.Fatalf("invalidated adjudication = %#v", escalation)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("upstream retry unexpectedly executed Fable: %v", err)
	}
	if !strings.Contains(progress.String(), "retry reviewer claude-escalation explicitly") {
		t.Fatalf("missing explicit dependent-phase guidance:\n%s", progress.String())
	}
}

func TestUpstreamRetryDefersInvalidatedCrossExamination(t *testing.T) {
	repo, target := fableRetryTestTarget(t)
	binDir := t.TempDir()
	codexPath := filepath.Join(binDir, "codex")
	claudePath := filepath.Join(binDir, "claude")
	marker := filepath.Join(binDir, "claude-invoked")
	writeExecutable(t, codexPath, fakeDisputingCodexScript)
	writeExecutable(t, claudePath, failIfFableRunsScript(marker))

	cfg := fableRetryConfig(codexPath, claudePath)
	cfg.Escalation.AdjudicateDisagreements = false
	cfg.CrossExamineBlockingFindings = true
	approve := fableRetryApproval()
	var progress bytes.Buffer
	decision, err := (Runner{Version: "test", Progress: &progress}).RunWithOptions(context.Background(), repo, target, cfg, RunOptions{
		RetryReviewers: map[string]bool{"codex": true},
		ReuseReviewers: []model.ReviewerResult{{Reviewer: "claude", Status: "completed", Report: approve}},
		ReuseCrossExaminations: []model.ReviewerResult{{
			Reviewer: "claude-cross-examination", Status: "completed", Report: approve,
			EscalationCause: "blocking_cross_examination",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != model.StateIncomplete {
		t.Fatalf("upstream retry decision = %#v", decision)
	}
	manifest := latestFableRetryManifest(t, repo)
	cross := reviewerResultByName(t, manifest.CrossExaminations, "claude-cross-examination")
	if cross.Status != "deferred" || cross.FailureKind != "dependency_changed" || !cross.Retryable || !strings.Contains(cross.Error, "fresh targeted retry") {
		t.Fatalf("invalidated cross-examination = %#v", cross)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("upstream retry unexpectedly executed Fable: %v", err)
	}
	if !strings.Contains(progress.String(), "retry reviewer claude-cross-examination explicitly") {
		t.Fatalf("missing explicit dependent-phase guidance:\n%s", progress.String())
	}
}

func TestFailedReusedChecksSuppressOptionalFablePhases(t *testing.T) {
	tests := []struct {
		name              string
		configure         func(*config.Config)
		selected          map[string]bool
		reviewers         []model.ReviewerResult
		wantSecurityDefer bool
	}{
		{
			name: "targeted security",
			configure: func(cfg *config.Config) {
				cfg.Escalation.ForceSecuritySensitive = true
			},
			selected: map[string]bool{"claude-security": true},
			reviewers: []model.ReviewerResult{
				{Reviewer: "codex", Status: "completed", Report: fableRetryApproval()},
				{Reviewer: "claude", Status: "completed", Report: fableRetryApproval()},
			},
			wantSecurityDefer: true,
		},
		{
			name: "dispute adjudication and cross examination",
			configure: func(cfg *config.Config) {
				cfg.Escalation.AdjudicateDisagreements = true
				cfg.CrossExamineBlockingFindings = true
			},
			selected: map[string]bool{"claude-escalation": true, "claude-cross-examination": true},
			reviewers: []model.ReviewerResult{
				{Reviewer: "codex", Status: "completed", Report: fableRetryRequestChanges()},
				{Reviewer: "claude", Status: "completed", Report: fableRetryApproval()},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, target := fableRetryTestTarget(t)
			binDir := t.TempDir()
			marker := filepath.Join(binDir, "claude-invoked")
			claudePath := filepath.Join(binDir, "claude")
			writeExecutable(t, claudePath, failIfFableRunsScript(marker))
			cfg := fableRetryConfig(filepath.Join(binDir, "codex-must-not-run"), claudePath)
			tt.configure(&cfg)
			parent := fableRetryParent(t, repo, target)
			decision, err := (Runner{Version: "test"}).RunWithOptions(context.Background(), repo, target, cfg, RunOptions{
				ParentRunID: parent.ID, RetryReviewers: tt.selected, ReuseReviewers: tt.reviewers,
				ReuseChecks: true, Checks: []model.CheckResult{{Name: "trusted-test", Status: "failed"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.State != model.StateChangesRequested || decision.Checks["trusted-test"] != "failed" {
				t.Fatalf("failed-check decision = %#v", decision)
			}
			manifest := latestFableRetryManifest(t, repo)
			if len(manifest.Checks) != 1 || manifest.Checks[0].ReusedFromRunID != parent.ID {
				t.Fatalf("check was not reused from parent: %#v", manifest.Checks)
			}
			if tt.wantSecurityDefer {
				security := reviewerResultByName(t, manifest.SecurityReviews, "claude-security")
				if security.Status != "deferred" || security.FailureKind != "outcome_fixed" {
					t.Fatalf("security phase was not cost-deferred: %#v", security)
				}
			} else if hasFableRetryResult(manifest.Reviewers, "claude-escalation") || hasFableRetryResult(manifest.CrossExaminations, "claude-cross-examination") {
				t.Fatalf("failed check should suppress adjudication and cross-examination: reviewers=%#v cross=%#v", manifest.Reviewers, manifest.CrossExaminations)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("failed reused check did not suppress Fable execution: %v", err)
			}
		})
	}
}

func fableRetryTestTarget(t *testing.T) (gitx.Repo, model.Target) {
	t.Helper()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", t.TempDir())
	root := orchestratorTestRepo(t)
	gitRun(t, root, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "app.txt")
	gitRun(t, root, "commit", "-m", "feat(app): add feature")
	repo, err := gitx.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveTarget(context.Background(), gitx.TargetOptions{Base: "main", RequireClean: true})
	if err != nil {
		t.Fatal(err)
	}
	return repo, target
}

func fableRetryConfig(codexPath, claudePath string) config.Config {
	cfg := config.Defaults()
	cfg.Reviewers.Codex.Command = codexPath
	cfg.Reviewers.Claude.Command = claudePath
	cfg.ReviewerTimeout.Duration = 5 * time.Second
	cfg.OverallTimeout.Duration = 10 * time.Second
	return cfg
}

func fableRetryApproval() *model.ReviewReport {
	return &model.ReviewReport{SchemaVersion: "1", Verdict: "approve", ContextComplete: true, Summary: "approved", ReviewedPaths: []string{"app.txt"}}
}

func fableRetryRequestChanges() *model.ReviewReport {
	return &model.ReviewReport{
		SchemaVersion: "1", Verdict: "request_changes", ContextComplete: true, Summary: "changes needed",
		Findings: []model.Finding{{
			ID: "major-1", Severity: "major", Confidence: 0.95, File: "app.txt", Line: 2,
			Claim: "The feature is disputed", Evidence: "The fixture intentionally disagrees", SuggestedFix: "Resolve the dispute",
			Reachability: &model.Reachability{Status: "demonstrated", Trigger: "feature input", Path: []string{"app.txt:2 applies it"}, Impact: "observable", Preconditions: []string{"enabled"}},
		}},
		ReviewedPaths: []string{"app.txt"},
	}
}

func fableRetryParent(t *testing.T, repo gitx.Repo, target model.Target) record.Run {
	t.Helper()
	store := record.New(repo.CommonDir)
	run, err := store.Create(time.Now().UTC().Add(-time.Hour), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{RunID: run.ID, Target: target}); err != nil {
		t.Fatal(err)
	}
	return run
}

func latestFableRetryManifest(t *testing.T, repo gitx.Repo) model.Manifest {
	t.Helper()
	run, err := record.New(repo.CommonDir).Resolve("latest")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := record.LoadManifest(run)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func reviewerResultByName(t *testing.T, results []model.ReviewerResult, name string) model.ReviewerResult {
	t.Helper()
	for _, result := range results {
		if result.Reviewer == name {
			return result
		}
	}
	t.Fatalf("reviewer %s not found in %#v", name, results)
	return model.ReviewerResult{}
}

func hasFableRetryResult(results []model.ReviewerResult, name string) bool {
	for _, result := range results {
		if result.Reviewer == name {
			return true
		}
	}
	return false
}

func failIfFableRunsScript(marker string) string {
	return "#!/bin/sh\nprintf invoked > '" + marker + "'\nexit 97\n"
}
