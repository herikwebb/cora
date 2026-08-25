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
	if groupID, err := syscall.Getpgid(command.Process.Pid); err == nil {
		_ = syscall.Kill(-groupID, syscall.SIGKILL)
		return
	}
	_ = command.Process.Kill()
}
