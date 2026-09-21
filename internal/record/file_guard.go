package record

import (
	"fmt"
	"os"
	"sync"
)

// fileGuard combines a process-local mutex with an operating-system advisory
// lock. The local mutex is required because advisory-lock semantics for two
// independently opened descriptors in one process differ across platforms.
// The OS lock serializes the same transition across CORA processes and is
// released automatically if a process exits unexpectedly.
type fileGuard struct {
	file  *os.File
	local *sync.Mutex
}

var localFileGuards sync.Map

func acquireFileGuard(path string) (*fileGuard, error) {
	value, _ := localFileGuards.LoadOrStore(path, &sync.Mutex{})
	local := value.(*sync.Mutex)
	local.Lock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		local.Unlock()
		return nil, fmt.Errorf("open acquisition guard: %w", err)
	}
	if err := file.Chmod(privateFileMode); err != nil {
		_ = file.Close()
		local.Unlock()
		return nil, fmt.Errorf("secure acquisition guard: %w", err)
	}
	if err := lockGuardFile(file); err != nil {
		_ = file.Close()
		local.Unlock()
		return nil, fmt.Errorf("lock acquisition guard: %w", err)
	}
	return &fileGuard{file: file, local: local}, nil
}

func (g *fileGuard) Release() {
	if g == nil {
		return
	}
	// Closing the descriptor also releases the OS lock. Attempt the explicit
	// unlock first, but never leave the process-local mutex held if it fails.
	_ = unlockGuardFile(g.file)
	_ = g.file.Close()
	g.local.Unlock()
}
