package autofix

import (
	"testing"

	"github.com/herikwebb/cora/internal/model"
)

func TestBaselineChecksAllowAdditionalValidatedImportedEvidence(t *testing.T) {
	configured := model.CheckResult{Name: "unit", Profile: "go", Status: "passed"}
	imported := model.CheckResult{
		Name: "evidence:ci", Profile: "imported-evidence", Status: "passed",
		ImportedEvidence: &model.ImportedValidationEvidence{Name: "ci"},
	}
	policy := []model.AutoFixCheckPolicy{{Name: "unit", Profile: "go"}}
	if !baselineChecksMatchPolicy([]model.CheckResult{configured, imported}, policy) {
		t.Fatal("valid imported evidence should be additive to configured policy checks")
	}
	if baselineChecksMatchPolicy([]model.CheckResult{imported}, policy) {
		t.Fatal("imported evidence must not hide a missing configured policy check")
	}
	if !baselineChecksMatchPolicy([]model.CheckResult{imported}, nil) {
		t.Fatal("a previously validated imported-only baseline should remain eligible")
	}
}
