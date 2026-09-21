//go:build !windows

package provider

import (
	"os"
	"syscall"
)

func readRecoveryCheckpoint(path string) ([]byte, error) {
	// O_NOFOLLOW rejects a replaced symlink, while O_NONBLOCK makes a replaced
	// FIFO inspectable without waiting forever for an attacker-controlled writer.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, syscall.EBADF
	}
	defer file.Close()
	return readBoundedRegularRecoveryFile(file)
}
