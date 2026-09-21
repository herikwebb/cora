//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	// Setpgid makes the child's PID its process-group ID. Address that known
	// group directly: Getpgid can race with a fast-exiting parent and otherwise
	// leave still-running grandchildren behind.
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	_ = command.Process.Kill()
}
