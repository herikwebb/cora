//go:build windows

package record

import (
	"errors"
	"os"
	"syscall"
)

func openValidationEvidenceFile(path string) (*os.File, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPointer,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var handleInfo syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		_ = syscall.CloseHandle(handle)
		return nil, err
	}
	if handleInfo.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = syscall.CloseHandle(handle)
		return nil, errors.New("validation evidence is a reparse point")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, syscall.EBADF
	}
	return file, nil
}
