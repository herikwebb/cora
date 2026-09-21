//go:build windows

package record

import (
	"errors"
	"syscall"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err == nil {
		_ = syscall.CloseHandle(handle)
		return true
	}
	// Access can be denied for a protected process even though it exists.
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
