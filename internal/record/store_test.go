package record

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestLatestUsesRunStartOrderInsteadOfCompletionOrder(t *testing.T) {
	store := New(t.TempDir())
	first, err := store.Create(time.Date(2026, 8, 25, 12, 0, 0, 1, time.UTC), strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(time.Date(2026, 8, 25, 12, 0, 0, 2, time.UTC), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(second); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(first); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Resolve("latest")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest = %s, want newest-started %s", latest.ID, second.ID)
	}
}

func TestProviderLeaseQueuesAtConcurrencyLimit(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	first, err := AcquireProvider(context.Background(), provider, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireProvider(ctx, provider, 1, nil); err == nil {
		t.Fatal("expected second provider lease to queue until its context expired")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireProvider(context.Background(), provider, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
