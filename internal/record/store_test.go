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

func TestProviderLeaseQueueIsFIFOAndReportsETA(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	first, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "first"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	held, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "held"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type acquired struct {
		name  string
		lease ProviderLease
		err   error
	}
	results := make(chan acquired, 2)
	start := func(name string) {
		go func() {
			lease, acquireErr := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: name}, nil)
			results <- acquired{name: name, lease: lease, err: acquireErr}
		}()
	}
	start("second")
	waitForTicketCount(t, filepath.Join(root, safeComponent(provider)), 1)
	start("third")
	waitForTicketCount(t, filepath.Join(root, safeComponent(provider)), 2)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	second := <-results
	if second.err != nil || second.name != "second" {
		t.Fatalf("first queued acquisition = %#v", second)
	}
	select {
	case unexpected := <-results:
		t.Fatalf("third acquired before second released: %#v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
	if err := second.lease.Release(); err != nil {
		t.Fatal(err)
	}
	third := <-results
	if third.err != nil || third.name != "third" {
		t.Fatalf("second queued acquisition = %#v", third)
	}
	if err := third.lease.Release(); err != nil {
		t.Fatal(err)
	}

	active, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "active"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	statusChannel := make(chan model.ProviderQueueStatus, 1)
	go func() {
		_, _ = AcquireProviderQueued(ctx, provider, 1, ProviderQueueRequest{Reviewer: "eta"}, func(status model.ProviderQueueStatus) {
			select {
			case statusChannel <- status:
			default:
			}
		})
	}()
	status := <-statusChannel
	cancel()
	_ = active.Release()
	if status.Position != 1 || status.Active != 1 || status.ETAAt == nil {
		t.Fatalf("queue status = %#v", status)
	}
}

func waitForTicketCount(t *testing.T, directory string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(directory)
		count := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ticket-") {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provider ticket count did not reach %d", want)
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
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", t.TempDir())
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
