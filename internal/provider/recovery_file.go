package provider

import (
	"errors"
	"io"
	"os"
)

// Recovery checkpoints are reviewer-writable hints. Keep their maximum well
// above a normal report while preventing a malicious worktree from turning
// timeout recovery into an unbounded allocation.
const maxRecoveryCheckpointBytes = 4 << 20

func readBoundedRegularRecoveryFile(file *os.File) ([]byte, error) {
	if file == nil {
		return nil, errors.New("recovery checkpoint is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("recovery checkpoint is not a regular file")
	}
	if info.Size() > maxRecoveryCheckpointBytes {
		return nil, errors.New("recovery checkpoint exceeds the size limit")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxRecoveryCheckpointBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maxRecoveryCheckpointBytes {
		return nil, errors.New("recovery checkpoint exceeds the size limit")
	}
	return contents, nil
}
