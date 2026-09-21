package process

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAllWritable removes a private temporary tree, repairing directory
// permissions once when untrusted code made its own workspace non-traversable.
// WalkDir does not follow symlinks, so the repair remains confined to path.
func RemoveAllWritable(path string) error {
	if path == "" {
		return nil
	}
	firstErr := os.RemoveAll(path)
	if firstErr == nil {
		return nil
	}
	repairErr := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return nil
	})
	if repairErr != nil && !errors.Is(repairErr, os.ErrNotExist) {
		return errors.Join(firstErr, fmt.Errorf("repair temporary directory permissions: %w", repairErr))
	}
	if retryErr := os.RemoveAll(path); retryErr != nil {
		return errors.Join(firstErr, fmt.Errorf("remove temporary directory after permission repair: %w", retryErr))
	}
	return nil
}
