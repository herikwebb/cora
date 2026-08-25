package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestStoreLifecycleAndLock(t *testing.T) {
	store := New(t.TempDir())
	lock, err := store.Acquire("abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("abcdef"); err == nil {
		t.Fatal("expected duplicate lock to fail")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	run, err := store.Create(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	decision := model.Decision{SchemaVersion: "1", RunID: run.ID, State: model.StateApproved}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(run); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Resolve("latest")
	if err != nil || latest.ID != run.ID {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	loaded, err := LoadDecision(latest)
	if err != nil || loaded.State != model.StateApproved {
		t.Fatalf("decision = %#v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "latest")); err != nil {
		t.Fatal(err)
	}
}
