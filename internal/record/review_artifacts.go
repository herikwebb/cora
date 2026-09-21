package record

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/webevidence"
)

const (
	maxRecordedPromptBytes   = 2 << 20
	maxRecordedDecisionBytes = 8 << 20
)

// ValidateCanonicalPatch proves that target.diff still represents the exact
// target named by the record.
func ValidateCanonicalPatch(run Run, target model.Target) error {
	return validateReviewArtifactHash(filepath.Join(run.Path, "target.diff"), target.DiffHash)
}

// ValidateReviewArtifacts re-establishes the integrity of every immutable
// input recorded for a review before its results are replayed or trusted.
func (s Store) ValidateReviewArtifacts(run Run, manifest model.Manifest) error {
	return s.validateReviewArtifacts(run, manifest, make(map[string]bool))
}

func (s Store) validateReviewArtifacts(run Run, manifest model.Manifest, visiting map[string]bool) error {
	if run.ID == "" || run.Path == "" || manifest.RunID != run.ID {
		return errors.New("review manifest does not identify its run")
	}
	isWebCollection := IsWebEvidenceRun(run)
	if isWebCollection != (manifest.WebEvidence != nil) {
		return errors.New("web evidence metadata does not match the run record collection")
	}
	if manifest.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("unsupported review manifest schema version %q", manifest.SchemaVersion)
	}
	if visiting[run.ID] {
		return fmt.Errorf("web evidence replay lineage contains a cycle at run %s", run.ID)
	}
	visiting[run.ID] = true
	defer delete(visiting, run.ID)

	artifacts := []struct {
		name string
		hash string
	}{
		{name: "target.diff", hash: manifest.Target.DiffHash},
		{name: "prompt.md", hash: manifest.PromptHash},
		{name: "policy.md", hash: manifest.PolicyHash},
		{name: "review.schema.json", hash: manifest.SchemaHash},
	}
	if manifest.SecurityPromptHash != "" {
		artifacts = append(artifacts, struct {
			name string
			hash string
		}{name: "security-review.prompt.md", hash: manifest.SecurityPromptHash})
	}
	if manifest.CrossExamPromptHash != "" {
		artifacts = append(artifacts, struct {
			name string
			hash string
		}{name: "cross-examination.prompt.md", hash: manifest.CrossExamPromptHash})
	}
	if manifest.FullTarget != nil {
		artifacts = append(artifacts, struct {
			name string
			hash string
		}{name: "full-target.diff", hash: manifest.FullTarget.DiffHash})
	}
	if isWebCollection && !manifest.FinishedAt.IsZero() && manifest.DecisionHash == "" {
		return errors.New("finished web evidence record is missing its decision hash")
	}
	if manifest.DecisionHash != "" {
		artifacts = append(artifacts, struct {
			name string
			hash string
		}{name: "decision.json", hash: manifest.DecisionHash})
	}
	for _, artifact := range artifacts {
		if err := validateReviewArtifactHash(filepath.Join(run.Path, artifact.name), artifact.hash); err != nil {
			return fmt.Errorf("validate recorded %s: %w", artifact.name, err)
		}
	}
	if manifest.DecisionHash != "" {
		if err := validateDecisionBinding(run, manifest); err != nil {
			return err
		}
	}

	if _, err := webevidence.EffectiveReviewScope(manifest); err != nil {
		return err
	}
	if err := ValidateImportedValidationEvidence(run, manifest.Target, manifest.RepositoryIdentity, manifest.Checks); err != nil {
		return fmt.Errorf("validate imported evidence: %w", err)
	}
	if err := webevidence.Validate(run.Path, manifest.WebEvidence); err != nil {
		return fmt.Errorf("validate web evidence: %w", err)
	}
	if err := webevidence.ValidateReviewerBindings(manifest.WebEvidence, manifest.Reviewers, manifest.SecurityReviews, manifest.CrossExaminations); err != nil {
		return err
	}
	prompt, err := readReviewArtifact(filepath.Join(run.Path, "prompt.md"), maxRecordedPromptBytes)
	if err != nil {
		return fmt.Errorf("read recorded prompt: %w", err)
	}
	if err := webevidence.ValidatePromptBinding(run.Path, manifest.WebEvidence, prompt); err != nil {
		return err
	}

	if manifest.WebEvidence == nil {
		return nil
	}
	snapshot := manifest.WebEvidence
	switch snapshot.Mode {
	case "captured":
		if manifest.ParentRunID != "" {
			return errors.New("captured web evidence cannot name a parent run")
		}
	case "replayed":
		if snapshot.SourceRunID != manifest.ParentRunID || manifest.ParentRunID == "" {
			return errors.New("replayed web evidence is not bound to the review parent")
		}
		parent, err := s.Resolve(manifest.ParentRunID)
		if err != nil {
			return fmt.Errorf("resolve web evidence source run: %w", err)
		}
		parentManifest, err := LoadManifest(parent)
		if err != nil {
			return fmt.Errorf("load web evidence source manifest: %w", err)
		}
		if parentManifest.FinishedAt.IsZero() {
			return errors.New("web evidence source run did not finish")
		}
		if parentManifest.RepositoryIdentity != manifest.RepositoryIdentity ||
			!sameExactTarget(parentManifest.Target, manifest.Target) {
			return errors.New("web evidence source run belongs to a different repository or target")
		}
		if err := s.validateReviewArtifacts(parent, parentManifest, visiting); err != nil {
			return fmt.Errorf("validate web evidence source run: %w", err)
		}
		if parentManifest.WebEvidence == nil || parentManifest.WebEvidence.SnapshotSHA256 != snapshot.SnapshotSHA256 {
			return errors.New("replayed web evidence differs from its parent snapshot")
		}
	}
	return nil
}

func validateDecisionBinding(run Run, manifest model.Manifest) error {
	contents, err := readReviewArtifact(filepath.Join(run.Path, "decision.json"), maxRecordedDecisionBytes)
	if err != nil {
		return fmt.Errorf("read recorded decision: %w", err)
	}
	var decision model.Decision
	if err := json.Unmarshal(contents, &decision); err != nil {
		return fmt.Errorf("parse recorded decision: %w", err)
	}
	if decision.SchemaVersion != manifest.SchemaVersion || decision.RunID != manifest.RunID ||
		decision.BaseSHA != manifest.Target.BaseSHA || decision.HeadSHA != manifest.Target.HeadSHA ||
		decision.DiffHash != manifest.Target.DiffHash || decision.StrictPolicy != manifest.StrictPolicy {
		return errors.New("recorded decision is not bound to its manifest and exact target")
	}
	return nil
}

func validateReviewArtifactHash(path, expected string) error {
	if len(expected) != sha256.Size*2 {
		return errors.New("recorded hash is missing or malformed")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("recorded hash is malformed")
	}
	file, err := openValidationEvidenceFile(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return errors.New("artifact is not a regular file")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("artifact does not match its recorded hash")
	}
	return nil
}

func readReviewArtifact(path string, limit int64) ([]byte, error) {
	file, err := openValidationEvidenceFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact is not a regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("artifact exceeds %d bytes", limit)
	}
	return contents, nil
}
