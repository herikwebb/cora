//go:build windows

package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunCancellationBoundsWindowsTreeTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	result := Run(ctx, Spec{
		Command: "cmd",
		Args:    []string{"/c", "ping -n 30 127.0.0.1 >NUL"},
		Env:     ReviewerEnvironment(false),
	})
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("Windows cancellation result = %#v", result)
	}
	maximum := taskkillTimeout + processWaitDelay + 3*time.Second
	if elapsed := time.Since(started); elapsed > maximum {
		t.Fatalf("Windows process-tree termination exceeded %s: %s", maximum, elapsed)
	}
}
