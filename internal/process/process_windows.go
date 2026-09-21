//go:build windows

package process

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

const taskkillTimeout = 2 * time.Second

func configureProcess(command *exec.Cmd) {}

func terminateProcess(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	// Process.Kill only terminates the direct child on Windows. taskkill /T
	// tears down the complete reviewer/check process tree first. Bound taskkill
	// itself, and retain a direct kill as a guaranteed fallback attempt.
	defer func() { _ = command.Process.Kill() }()
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	treeKill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid))
	treeKill.WaitDelay = processWaitDelay
	_ = treeKill.Run()
}
