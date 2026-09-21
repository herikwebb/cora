//go:build !windows

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestInterruptContextRestoresDefaultBehaviorAfterFirstSignal(t *testing.T) {
	if os.Getenv("CORA_SIGNAL_HELPER") == "1" {
		ctx, stop := interruptContext(context.Background(), syscall.SIGTERM)
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("canceled")
		select {}
	}

	command := exec.Command(os.Args[0], "-test.run=^TestInterruptContextRestoresDefaultBehaviorAfterFirstSignal$")
	command.Env = append(os.Environ(), "CORA_SIGNAL_HELPER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	lines := make(chan string, 2)
	scanDone := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanDone <- scanner.Err()
	}()
	waitLine := func(want string) {
		t.Helper()
		select {
		case line := <-lines:
			if line != want {
				t.Fatalf("signal helper line = %q, want %q", line, want)
			}
		case err := <-scanDone:
			t.Fatalf("signal helper output ended before %q: %v", want, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for signal helper line %q", want)
		}
	}
	waitLine("ready")
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitLine("canceled")
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case waitErr := <-waited:
		finished = true
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("second signal did not terminate helper: %v", waitErr)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Fatalf("helper termination status = %#v", exitErr.Sys())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second signal did not use the restored default behavior")
	}
}
