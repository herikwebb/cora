//go:build !windows

package provider

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadValidatedCheckpointRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	checkpoint := filepath.Join(directory, "checkpoint.json")
	writeValidRecoveryCheckpoint(t, target)
	if err := os.Symlink(target, checkpoint); err != nil {
		t.Fatal(err)
	}
	if report, found := readValidatedCheckpoint(checkpoint); found {
		t.Fatalf("symlink checkpoint accepted: %#v", report)
	}
}

func TestReadValidatedCheckpointRejectsFIFOWithoutBlocking(t *testing.T) {
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.fifo")
	if err := syscall.Mkfifo(checkpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan bool, 1)
	go func() {
		_, found := readValidatedCheckpoint(checkpoint)
		finished <- found
	}()
	select {
	case found := <-finished:
		if found {
			t.Fatal("FIFO checkpoint was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO checkpoint blocked recovery")
	}
}

func writeValidRecoveryCheckpoint(t *testing.T, path string) {
	t.Helper()
	contents := []byte(`{"schema_version":"1","verdict":"abstain","context_complete":false,"summary":"partial","findings":[],"reviewed_paths":[],"omitted_paths":["app.go"],"residual_risks":[]}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
