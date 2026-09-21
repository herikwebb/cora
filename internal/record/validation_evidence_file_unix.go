//go:build !windows

package record

import (
	"os"
	"syscall"
)

func openValidationEvidenceFile(path string) (*os.File, error) {
	// O_NOFOLLOW closes the Lstat-to-open symlink race. O_NONBLOCK ensures a
	// path swapped to a FIFO cannot stall an evidence import indefinitely.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, syscall.EBADF
	}
	return file, nil
}
