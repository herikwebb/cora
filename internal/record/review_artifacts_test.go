package record

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestValidateReviewArtifactsRejectsTamperedCoreArtifacts(t *testing.T) {
	store := New(t.TempDir())
	run, err := store.Create(time.Unix(1, 0), "head")
	if err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		"target.diff":        []byte("diff --git a/app.go b/app.go\n"),
		"prompt.md":          []byte("review the exact target\n"),
		"policy.md":          []byte("network access prohibited\n"),
		"review.schema.json": []byte(`{"type":"object"}`),
	}
	for name, contents := range artifacts {
		if err := WriteFile(filepath.Join(run.Path, name), contents); err != nil {
			t.Fatal(err)
		}
	}
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: artifactHash(artifacts["target.diff"])}
	decision := model.Decision{
		SchemaVersion: model.SchemaVersion,
		RunID:         run.ID,
		BaseSHA:       target.BaseSHA,
		HeadSHA:       target.HeadSHA,
		DiffHash:      target.DiffHash,
		State:         model.StateApproved,
	}
	decisionHash, err := WriteHashedJSON(filepath.Join(run.Path, "decision.json"), decision)
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.Manifest{
		SchemaVersion: model.SchemaVersion,
		RunID:         run.ID,
		FinishedAt:    time.Unix(2, 0),
		ReviewScope:   "full",
		Target:        target,
		PromptHash:    artifactHash(artifacts["prompt.md"]),
		PolicyHash:    artifactHash(artifacts["policy.md"]),
		SchemaHash:    artifactHash(artifacts["review.schema.json"]),
		DecisionHash:  decisionHash,
	}
	if err := store.ValidateReviewArtifacts(run, manifest); err != nil {
		t.Fatalf("validate intact artifacts: %v", err)
	}

	for _, name := range []string{"target.diff", "prompt.md", "policy.md", "review.schema.json", "decision.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(run.Path, name)
			original, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if writeErr := os.WriteFile(path, append(append([]byte(nil), original...), 'x'), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			validationErr := store.ValidateReviewArtifacts(run, manifest)
			if validationErr == nil || !strings.Contains(validationErr.Error(), name) {
				t.Fatalf("tampered %s validation error = %v", name, validationErr)
			}
			if writeErr := os.WriteFile(path, original, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		})
	}

	decision.HeadSHA = "different-head"
	tamperedHash, err := WriteHashedJSON(filepath.Join(run.Path, "decision.json"), decision)
	if err != nil {
		t.Fatal(err)
	}
	tamperedManifest := manifest
	tamperedManifest.DecisionHash = tamperedHash
	if err := store.ValidateReviewArtifacts(run, tamperedManifest); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("semantically mismatched decision validation error = %v", err)
	}
}

func artifactHash(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
