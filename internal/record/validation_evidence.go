package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

const (
	validationEvidenceSchemaVersion = "1"
	validationEvidenceMaxBytes      = 1 << 20
	validationEvidenceTrust         = "operator-supplied-attestation"
	validationEvidenceIsolation     = "imported-evidence-no-execution"
)

type validationEvidenceAttestation struct {
	SchemaVersion      string    `json:"schema_version"`
	Name               string    `json:"name"`
	RepositoryIdentity string    `json:"repository_identity"`
	BaseSHA            string    `json:"base_sha"`
	HeadSHA            string    `json:"head_sha"`
	DiffHash           string    `json:"diff_hash"`
	Status             string    `json:"status"`
	VerifiedAt         time.Time `json:"verified_at"`
	Verifier           string    `json:"verifier"`
	Source             string    `json:"source"`
	Command            []string  `json:"command"`
	Summary            string    `json:"summary"`
}

// ImportValidationEvidence validates an operator-selected external
// attestation, copies its exact bytes into the private run record, and returns
// a normal passed check result. It never executes the attested command. The
// caller is responsible for deciding whether it trusts the named verifier.
func ImportValidationEvidence(run Run, sourcePath string, target model.Target, repositoryIdentity string) (model.CheckResult, error) {
	result, contents, err := inspectValidationEvidence(sourcePath, target, repositoryIdentity)
	if err != nil {
		return model.CheckResult{}, err
	}
	if err := WriteFile(filepath.Join(run.Path, filepath.FromSlash(result.ImportedEvidence.RecordFile)), contents); err != nil {
		return model.CheckResult{}, fmt.Errorf("preserve validation evidence %q: %w", sourcePath, err)
	}
	return result, nil
}

// InspectValidationEvidence performs the same strict parsing and exact-target
// validation as import without creating or modifying a run record. It supports
// read-only review planning; a real review re-reads and imports the file.
func InspectValidationEvidence(sourcePath string, target model.Target, repositoryIdentity string) (model.CheckResult, error) {
	result, _, err := inspectValidationEvidence(sourcePath, target, repositoryIdentity)
	return result, err
}

func inspectValidationEvidence(sourcePath string, target model.Target, repositoryIdentity string) (model.CheckResult, []byte, error) {
	contents, err := readValidationEvidenceFile(sourcePath)
	if err != nil {
		return model.CheckResult{}, nil, fmt.Errorf("read validation evidence %q: %w", sourcePath, err)
	}
	attestation, err := decodeValidationEvidence(contents)
	if err != nil {
		return model.CheckResult{}, nil, fmt.Errorf("parse validation evidence %q: %w", sourcePath, err)
	}
	if err := validateValidationEvidence(attestation, target, repositoryIdentity, time.Now()); err != nil {
		return model.CheckResult{}, nil, fmt.Errorf("validate validation evidence %q: %w", sourcePath, err)
	}

	digest := sha256.Sum256(contents)
	contentHash := hex.EncodeToString(digest[:])
	recordFile := filepath.ToSlash(filepath.Join("validation-evidence", attestation.Name+"-"+contentHash[:12]+".json"))
	metadata := importedValidationEvidence(attestation, contentHash, recordFile)
	return model.CheckResult{
		Name: "evidence:" + attestation.Name, Profile: "imported-evidence", Status: "passed",
		Isolation: validationEvidenceIsolation, ImportedEvidence: &metadata,
	}, contents, nil
}

// ValidateImportedValidationEvidence revalidates every imported check against
// its private source artifact. Retry, verification, and approval-lineage code
// use this before trusting a recorded passed status.
func ValidateImportedValidationEvidence(run Run, target model.Target, repositoryIdentity string, checks []model.CheckResult) error {
	for _, check := range checks {
		metadata := check.ImportedEvidence
		if metadata == nil {
			// Isolation is assigned by Cora and uniquely identifies an imported
			// attestation. Profile names are user-configurable, so a normal executed
			// check may legitimately use "imported-evidence" as its profile name.
			if check.Isolation == validationEvidenceIsolation {
				return fmt.Errorf("imported validation check %q is missing its evidence metadata", check.Name)
			}
			continue
		}
		if check.Name != "evidence:"+metadata.Name || check.Profile != "imported-evidence" || check.Status != "passed" || check.Isolation != validationEvidenceIsolation {
			return fmt.Errorf("imported validation check %q has inconsistent result metadata", check.Name)
		}
		if metadata.Trust != validationEvidenceTrust {
			return fmt.Errorf("imported validation check %q has unknown trust classification %q", check.Name, metadata.Trust)
		}
		if len(metadata.ContentSHA256) != sha256.Size*2 {
			return fmt.Errorf("imported validation check %q has an invalid content hash", check.Name)
		}
		expectedRecordFile := filepath.ToSlash(filepath.Join("validation-evidence", metadata.Name+"-"+metadata.ContentSHA256[:12]+".json"))
		if metadata.RecordFile != expectedRecordFile {
			return fmt.Errorf("imported validation check %q has an inconsistent record file", check.Name)
		}
		recordPath, err := validationEvidenceRecordPath(run, metadata.RecordFile)
		if err != nil {
			return fmt.Errorf("imported validation check %q: %w", check.Name, err)
		}
		contents, err := readValidationEvidenceFile(recordPath)
		if err != nil {
			return fmt.Errorf("read recorded validation evidence for %q: %w", check.Name, err)
		}
		digest := sha256.Sum256(contents)
		contentHash := hex.EncodeToString(digest[:])
		if contentHash != metadata.ContentSHA256 {
			return fmt.Errorf("recorded validation evidence for %q does not match its content hash", check.Name)
		}
		attestation, err := decodeValidationEvidence(contents)
		if err != nil {
			return fmt.Errorf("parse recorded validation evidence for %q: %w", check.Name, err)
		}
		if err := validateValidationEvidence(attestation, target, repositoryIdentity, time.Now()); err != nil {
			return fmt.Errorf("revalidate recorded validation evidence for %q: %w", check.Name, err)
		}
		expected := importedValidationEvidence(attestation, contentHash, metadata.RecordFile)
		if !sameImportedValidationEvidence(*metadata, expected) {
			return fmt.Errorf("imported validation check %q does not match its recorded attestation", check.Name)
		}
	}
	return nil
}

// CopyImportedValidationEvidence preserves imported artifacts in a child run
// before its check results are reused. This keeps every retry self-contained
// and prevents a child manifest from referring implicitly to mutable parent
// state.
func CopyImportedValidationEvidence(from, to Run, target model.Target, repositoryIdentity string, checks []model.CheckResult) error {
	if err := ValidateImportedValidationEvidence(from, target, repositoryIdentity, checks); err != nil {
		return err
	}
	for _, check := range checks {
		if check.ImportedEvidence == nil {
			continue
		}
		source, err := validationEvidenceRecordPath(from, check.ImportedEvidence.RecordFile)
		if err != nil {
			return err
		}
		contents, err := readValidationEvidenceFile(source)
		if err != nil {
			return err
		}
		destination, err := validationEvidenceRecordPath(to, check.ImportedEvidence.RecordFile)
		if err != nil {
			return err
		}
		if err := WriteFile(destination, contents); err != nil {
			return err
		}
	}
	return ValidateImportedValidationEvidence(to, target, repositoryIdentity, checks)
}

func readValidationEvidenceFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path cannot be empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("evidence must be a regular file (symlinks and special files are rejected)")
	}
	if info.Size() > validationEvidenceMaxBytes {
		return nil, fmt.Errorf("evidence exceeds %d-byte limit", validationEvidenceMaxBytes)
	}
	file, err := openValidationEvidenceFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, errors.New("evidence changed to a non-regular file while being read")
	}
	contents, err := io.ReadAll(io.LimitReader(file, validationEvidenceMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > validationEvidenceMaxBytes {
		return nil, fmt.Errorf("evidence exceeds %d-byte limit", validationEvidenceMaxBytes)
	}
	return contents, nil
}

func decodeValidationEvidence(contents []byte) (validationEvidenceAttestation, error) {
	if len(bytes.TrimSpace(contents)) == 0 {
		return validationEvidenceAttestation{}, errors.New("evidence is empty")
	}
	if err := rejectDuplicateEvidenceFields(contents); err != nil {
		return validationEvidenceAttestation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var attestation validationEvidenceAttestation
	if err := decoder.Decode(&attestation); err != nil {
		return validationEvidenceAttestation{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return validationEvidenceAttestation{}, errors.New("evidence must contain exactly one JSON object")
		}
		return validationEvidenceAttestation{}, fmt.Errorf("decode trailing data: %w", err)
	}
	return attestation, nil
}

func rejectDuplicateEvidenceFields(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return errors.New("evidence must be a JSON object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("evidence contains a non-string field name")
		}
		if seen[name] {
			return fmt.Errorf("duplicate field %q", name)
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}

func validateValidationEvidence(attestation validationEvidenceAttestation, target model.Target, repositoryIdentity string, now time.Time) error {
	if attestation.SchemaVersion != validationEvidenceSchemaVersion {
		return fmt.Errorf("schema_version must be %q", validationEvidenceSchemaVersion)
	}
	if !validValidationEvidenceName(attestation.Name) {
		return errors.New("name must contain 1-64 ASCII letters, digits, dots, underscores, or hyphens and start with a letter or digit")
	}
	if attestation.RepositoryIdentity != repositoryIdentity {
		return errors.New("repository_identity does not match the target repository")
	}
	if attestation.BaseSHA != target.BaseSHA {
		return errors.New("base_sha does not match the exact review target")
	}
	if attestation.HeadSHA != target.HeadSHA {
		return errors.New("head_sha does not match the exact review target")
	}
	if attestation.DiffHash != target.DiffHash {
		return errors.New("diff_hash does not match the exact review target")
	}
	if attestation.Status != "passed" {
		return errors.New("status must be \"passed\"")
	}
	if attestation.VerifiedAt.IsZero() {
		return errors.New("verified_at is required")
	}
	if attestation.VerifiedAt.After(now.Add(5 * time.Minute)) {
		return errors.New("verified_at is implausibly far in the future")
	}
	if !nonemptyBounded(attestation.Verifier, 256) {
		return errors.New("verifier must contain 1-256 non-whitespace characters")
	}
	if !nonemptyBounded(attestation.Source, 2048) {
		return errors.New("source must contain 1-2048 non-whitespace characters")
	}
	if len(attestation.Command) == 0 || len(attestation.Command) > 128 {
		return errors.New("command must contain 1-128 arguments")
	}
	for _, argument := range attestation.Command {
		if !nonemptyBounded(argument, 8192) {
			return errors.New("command arguments must contain 1-8192 non-whitespace characters")
		}
	}
	if !nonemptyBounded(attestation.Summary, 8192) {
		return errors.New("summary must contain 1-8192 non-whitespace characters")
	}
	return nil
}

func validValidationEvidenceName(name string) bool {
	if len(name) == 0 || len(name) > 64 || !asciiAlphaNumeric(name[0]) {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if !asciiAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func nonemptyBounded(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum
}

func importedValidationEvidence(attestation validationEvidenceAttestation, contentHash, recordFile string) model.ImportedValidationEvidence {
	return model.ImportedValidationEvidence{
		SchemaVersion: attestation.SchemaVersion, Name: attestation.Name,
		RepositoryIdentity: attestation.RepositoryIdentity, BaseSHA: attestation.BaseSHA,
		HeadSHA: attestation.HeadSHA, DiffHash: attestation.DiffHash, Status: attestation.Status,
		VerifiedAt: attestation.VerifiedAt, Verifier: attestation.Verifier, Source: attestation.Source,
		Command: append([]string(nil), attestation.Command...), Summary: attestation.Summary,
		Trust: validationEvidenceTrust, ContentSHA256: contentHash, RecordFile: recordFile,
	}
}

func sameImportedValidationEvidence(left, right model.ImportedValidationEvidence) bool {
	if left.SchemaVersion != right.SchemaVersion || left.Name != right.Name ||
		left.RepositoryIdentity != right.RepositoryIdentity || left.BaseSHA != right.BaseSHA ||
		left.HeadSHA != right.HeadSHA || left.DiffHash != right.DiffHash || left.Status != right.Status ||
		!left.VerifiedAt.Equal(right.VerifiedAt) || left.Verifier != right.Verifier || left.Source != right.Source ||
		left.Summary != right.Summary || left.Trust != right.Trust || left.ContentSHA256 != right.ContentSHA256 ||
		left.RecordFile != right.RecordFile || len(left.Command) != len(right.Command) {
		return false
	}
	for index := range left.Command {
		if left.Command[index] != right.Command[index] {
			return false
		}
	}
	return true
}

func validationEvidenceRecordPath(run Run, recordFile string) (string, error) {
	if recordFile == "" || filepath.IsAbs(recordFile) || filepath.Clean(recordFile) != filepath.FromSlash(recordFile) {
		return "", errors.New("record_file is not a clean relative path")
	}
	prefix := "validation-evidence" + string(filepath.Separator)
	if !strings.HasPrefix(filepath.FromSlash(recordFile), prefix) {
		return "", errors.New("record_file escapes the validation-evidence directory")
	}
	return filepath.Join(run.Path, filepath.FromSlash(recordFile)), nil
}
