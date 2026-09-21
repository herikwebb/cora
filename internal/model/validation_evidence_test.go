package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestImportedValidationEvidenceRoundTripsWithCheckResult(t *testing.T) {
	verifiedAt := time.Date(2026, 9, 3, 14, 30, 0, 0, time.UTC)
	original := CheckResult{
		Name: "evidence:ci", Profile: "imported-evidence", Status: "passed", Isolation: "imported-evidence-no-execution",
		ImportedEvidence: &ImportedValidationEvidence{
			SchemaVersion: "1", Name: "ci", RepositoryIdentity: "github.com/example/project",
			BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), DiffHash: strings.Repeat("c", 64),
			Status: "passed", VerifiedAt: verifiedAt, Verifier: "github-actions", Source: "https://ci.example/1",
			Command: []string{"go", "test", "./..."}, Summary: "passed", Trust: "operator-supplied-attestation",
			ContentSHA256: strings.Repeat("d", 64), RecordFile: "validation-evidence/ci-dddddddddddd.json",
		},
	}
	contents, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CheckResult
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ImportedEvidence == nil || decoded.ImportedEvidence.Name != "ci" || !decoded.ImportedEvidence.VerifiedAt.Equal(verifiedAt) || decoded.ImportedEvidence.RecordFile != original.ImportedEvidence.RecordFile {
		t.Fatalf("round-tripped imported evidence = %#v", decoded.ImportedEvidence)
	}
}
