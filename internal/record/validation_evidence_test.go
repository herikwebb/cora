package record

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestImportValidationEvidenceBindsAndPrivatelyPreservesAttestation(t *testing.T) {
	store := New(t.TempDir())
	run, err := store.Create(time.Now(), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	target := validationEvidenceTarget()
	identity := "github.com/example/project"
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	attestation := validValidationEvidenceAttestation(target, identity)
	attestation.Command = []string{"touch", marker}
	contents := marshalValidationEvidence(t, attestation)
	source := filepath.Join(t.TempDir(), "ci.json")
	if err := os.WriteFile(source, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ImportValidationEvidence(run, source, target, identity)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "evidence:ci-unit" || result.Status != "passed" || result.Profile != "imported-evidence" || result.Isolation != "imported-evidence-no-execution" || result.ImportedEvidence == nil {
		t.Fatalf("imported check = %#v", result)
	}
	if result.ImportedEvidence.Trust != "operator-supplied-attestation" || result.ImportedEvidence.ContentSHA256 == "" {
		t.Fatalf("imported metadata = %#v", result.ImportedEvidence)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("recorded command was unexpectedly executed: %v", err)
	}
	recorded := filepath.Join(run.Path, filepath.FromSlash(result.ImportedEvidence.RecordFile))
	recordedContents, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if string(recordedContents) != string(contents) {
		t.Fatalf("recorded bytes changed:\n%s", recordedContents)
	}
	info, err := os.Stat(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 600", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(recorded))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("evidence directory mode = %o, want 700", directoryInfo.Mode().Perm())
	}
	if err := ValidateImportedValidationEvidence(run, target, identity, []model.CheckResult{result}); err != nil {
		t.Fatalf("validate imported evidence: %v", err)
	}

	child, err := store.Create(time.Now().Add(time.Second), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyImportedValidationEvidence(run, child, target, identity, []model.CheckResult{result}); err != nil {
		t.Fatalf("copy imported evidence: %v", err)
	}
	if err := ValidateImportedValidationEvidence(child, target, identity, []model.CheckResult{result}); err != nil {
		t.Fatalf("validate copied evidence: %v", err)
	}
	childPath := filepath.Join(child.Path, filepath.FromSlash(result.ImportedEvidence.RecordFile))
	if err := os.WriteFile(childPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImportedValidationEvidence(child, target, identity, []model.CheckResult{result}); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Fatalf("tampered evidence error = %v", err)
	}
}

func TestImportValidationEvidenceRejectsMalformedOrStaleInput(t *testing.T) {
	target := validationEvidenceTarget()
	identity := "github.com/example/project"
	store := New(t.TempDir())
	run, err := store.Create(time.Now(), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		edit func(*validationEvidenceAttestation)
		want string
	}{
		{name: "wrong repository", edit: func(value *validationEvidenceAttestation) { value.RepositoryIdentity = "github.com/other/project" }, want: "repository_identity"},
		{name: "stale base", edit: func(value *validationEvidenceAttestation) { value.BaseSHA = strings.Repeat("d", 40) }, want: "base_sha"},
		{name: "stale head", edit: func(value *validationEvidenceAttestation) { value.HeadSHA = strings.Repeat("e", 40) }, want: "head_sha"},
		{name: "stale diff", edit: func(value *validationEvidenceAttestation) { value.DiffHash = strings.Repeat("f", 64) }, want: "diff_hash"},
		{name: "nonpassing", edit: func(value *validationEvidenceAttestation) { value.Status = "failed" }, want: "status must"},
		{name: "future", edit: func(value *validationEvidenceAttestation) { value.VerifiedAt = time.Now().Add(time.Hour) }, want: "future"},
		{name: "missing verifier", edit: func(value *validationEvidenceAttestation) { value.Verifier = " " }, want: "verifier"},
		{name: "missing command", edit: func(value *validationEvidenceAttestation) { value.Command = nil }, want: "command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attestation := validValidationEvidenceAttestation(target, identity)
			test.edit(&attestation)
			path := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(path, marshalValidationEvidence(t, attestation), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ImportValidationEvidence(run, path, target, identity); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	unknown := filepath.Join(t.TempDir(), "unknown.json")
	contents := strings.TrimSuffix(string(marshalValidationEvidence(t, validValidationEvidenceAttestation(target, identity))), "}\n") + ",\n  \"unexpected\": true\n}\n"
	if err := os.WriteFile(unknown, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportValidationEvidence(run, unknown, target, identity); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}

	duplicate := filepath.Join(t.TempDir(), "duplicate.json")
	if err := os.WriteFile(duplicate, []byte(`{"schema_version":"1","schema_version":"1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportValidationEvidence(run, duplicate, target, identity); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate-field error = %v", err)
	}

	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(real, marshalValidationEvidence(t, validValidationEvidenceAttestation(target, identity)), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(real, symlink); err == nil {
		if _, err := ImportValidationEvidence(run, symlink, target, identity); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink error = %v", err)
		}
	} else {
		t.Logf("symlink creation unavailable: %v", err)
	}
	if _, err := ImportValidationEvidence(run, t.TempDir(), target, identity); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, make([]byte, validationEvidenceMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportValidationEvidence(run, oversized, target, identity); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestValidateImportedValidationEvidenceRejectsShapedCheckWithoutMetadata(t *testing.T) {
	err := ValidateImportedValidationEvidence(Run{Path: t.TempDir()}, validationEvidenceTarget(), "github.com/example/project", []model.CheckResult{{
		Name: "evidence:ci-unit", Profile: "imported-evidence", Status: "passed", Isolation: "imported-evidence-no-execution",
	}})
	if err == nil || !strings.Contains(err.Error(), "missing its evidence metadata") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateImportedValidationEvidenceAllowsOrdinaryCheckWithSameProfileName(t *testing.T) {
	err := ValidateImportedValidationEvidence(Run{Path: t.TempDir()}, validationEvidenceTarget(), "github.com/example/project", []model.CheckResult{{
		Name: "ordinary-check", Profile: "imported-evidence", Status: "passed", Isolation: "disposable-clone-minimal-env",
	}})
	if err != nil {
		t.Fatalf("ordinary check was mistaken for imported evidence: %v", err)
	}
}

func TestApprovedBaselineRevalidatesImportedEvidence(t *testing.T) {
	store := New(t.TempDir())
	run, err := store.Create(time.Now(), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/app.go b/app.go\n")
	digest := sha256.Sum256(patch)
	target := validationEvidenceTarget()
	target.DiffHash = hex.EncodeToString(digest[:])
	identity := "github.com/example/project"
	source := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(source, marshalValidationEvidence(t, validValidationEvidenceAttestation(target, identity)), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := ImportValidationEvidence(run, source, target, identity)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := model.ReviewerResult{
		Reviewer: "codex", Status: "completed", Attempt: 1,
		Report: &model.ReviewReport{Verdict: "approve", ContextComplete: true},
	}
	manifest := model.Manifest{
		RunID: run.ID, RepositoryIdentity: identity, ReviewScope: "full", Target: target,
		Reviewers: []model.ReviewerResult{reviewer}, Checks: []model.CheckResult{check},
	}
	decision := model.Decision{
		RunID: run.ID, State: model.StateApproved, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve"},
	}
	if err := WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedBaseline(run); err != nil {
		t.Fatalf("load valid baseline: %v", err)
	}
	artifact := filepath.Join(run.Path, filepath.FromSlash(check.ImportedEvidence.RecordFile))
	if err := os.WriteFile(artifact, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedBaseline(run); !errors.Is(err, ErrNotApprovedBaseline) {
		t.Fatalf("tampered baseline error = %v", err)
	}
}

func validationEvidenceTarget() model.Target {
	return model.Target{
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), DiffHash: strings.Repeat("c", 64), Finalizable: true,
	}
}

func validValidationEvidenceAttestation(target model.Target, identity string) validationEvidenceAttestation {
	return validationEvidenceAttestation{
		SchemaVersion: "1", Name: "ci-unit", RepositoryIdentity: identity,
		BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Status: "passed", VerifiedAt: time.Now().Add(-time.Minute).UTC(), Verifier: "github-actions",
		Source: "https://ci.example/runs/123", Command: []string{"go", "test", "./..."}, Summary: "All unit tests passed.",
	}
}

func marshalValidationEvidence(t *testing.T, value validationEvidenceAttestation) []byte {
	t.Helper()
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(contents, '\n')
}
