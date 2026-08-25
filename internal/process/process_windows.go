//go:build windows

package process

import "os/exec"

func configureProcess(command *exec.Cmd) {}

func terminateProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
