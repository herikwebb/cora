//go:build !windows

package record

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStoreCreatesPrivateRecordsWithPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	commonDir := t.TempDir()
	store := New(commonDir)
	if err := os.MkdirAll(store.Root, 0o755); err != nil {
		t.Fatal(err)
	}

	lock, err := store.Acquire("private-target")
	if err != nil {
		t.Fatal(err)
	}
	assertPermissions(t, store.Root, 0o700)
	assertPermissions(t, filepath.Join(store.Root, "locks"), 0o700)
	assertPermissions(t, lock.path, 0o600)

	run, err := store.Create(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	assertPermissions(t, filepath.Join(store.Root, "runs"), 0o700)
	assertPermissions(t, run.Path, 0o700)

	artifactPath := filepath.Join(run.Path, "target.diff")
	if err := WriteFile(artifactPath, []byte("private patch\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(run.Path, "manifest.json"), map[string]string{"secret": "value"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(run, map[string]string{"type": "run.started"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(run); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		artifactPath,
		filepath.Join(run.Path, "manifest.json"),
		filepath.Join(run.Path, "events.jsonl"),
		filepath.Join(store.Root, "latest"),
	} {
		assertPermissions(t, path, 0o600)
	}
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("permissions for %s = %#o, want %#o", path, got, want)
	}
}
