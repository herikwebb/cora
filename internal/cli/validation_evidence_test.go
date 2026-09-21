package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/record"
)

func TestReviewCommandExposesExplicitValidationEvidenceImport(t *testing.T) {
	command := newReviewCommand(&options{})
	flag := command.Flags().Lookup("validation-evidence")
	if flag == nil {
		t.Fatal("review command is missing --validation-evidence")
	}
	command.SetArgs([]string{"--auto-fix", "--validation-evidence", "ci.json"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be combined with --auto-fix") {
		t.Fatalf("auto-fix evidence error = %v", err)
	}
}

func TestFindApprovalRejectsTamperedImportedValidationEvidence(t *testing.T) {
	store := record.New(t.TempDir())
	run, err := store.Create(time.Now(), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/app.go b/app.go\n")
	digest := sha256.Sum256(patch)
	target := model.Target{
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), DiffHash: hex.EncodeToString(digest[:]), Finalizable: true,
	}
	identity := "github.com/example/project"
	contents, err := json.MarshalIndent(map[string]any{
		"schema_version": "1", "name": "ci", "repository_identity": identity,
		"base_sha": target.BaseSHA, "head_sha": target.HeadSHA, "diff_hash": target.DiffHash,
		"status": "passed", "verified_at": time.Now().Add(-time.Minute).UTC(), "verifier": "github-actions",
		"source": "https://ci.example/runs/123", "command": []string{"go", "test", "./..."}, "summary": "passed",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(source, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := record.ImportValidationEvidence(run, source, target, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	manifest := model.Manifest{RunID: run.ID, RepositoryIdentity: identity, Target: target, ReviewScope: "full", FinishedAt: time.Now(), Checks: []model.CheckResult{check}}
	decision := model.Decision{RunID: run.ID, State: model.StateApproved, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash}
	if err := record.WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := findApproval(store, run.ID, target.HeadSHA, identity); err != nil {
		t.Fatalf("valid approval not found: %v", err)
	}
	if _, _, _, err := findApproval(store, run.ID, target.HeadSHA, "github.com/other/project"); err == nil || !strings.Contains(err.Error(), "different repository identity") {
		t.Fatalf("repository identity mismatch error = %v", err)
	}
	recordedPath := filepath.Join(run.Path, filepath.FromSlash(check.ImportedEvidence.RecordFile))
	if err := os.WriteFile(recordedPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := findApproval(store, run.ID, target.HeadSHA, identity); err == nil || !strings.Contains(err.Error(), "invalid imported validation evidence") {
		t.Fatalf("tampered approval error = %v", err)
	}
}
