package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

type Store struct {
	Root string
}

type Run struct {
	ID   string
	Path string
}

type Lock struct {
	path string
}

type ProviderLease struct {
	path string
}

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

func New(commonDir string) Store {
	return Store{Root: filepath.Join(commonDir, "cora")}
}

func (s Store) Create(started time.Time, headSHA string) (Run, error) {
	if err := ensurePrivateDir(s.Root); err != nil {
		return Run{}, fmt.Errorf("secure CORA record directory: %w", err)
	}
	if err := ensurePrivateDir(filepath.Join(s.Root, "runs")); err != nil {
		return Run{}, fmt.Errorf("create CORA record directory: %w", err)
	}
	short := headSHA
	if len(short) > 8 {
		short = short[:8]
	}
	baseID := started.UTC().Format("20060102T150405.000000000Z") + "-" + short
	for attempt := 0; attempt < 100; attempt++ {
		id := baseID
		if attempt > 0 {
			id = fmt.Sprintf("%s-%02d", baseID, attempt)
		}
		path := filepath.Join(s.Root, "runs", id)
		if err := os.Mkdir(path, privateDirMode); err == nil {
			return Run{ID: id, Path: path}, nil
		} else if !errors.Is(err, os.ErrExist) {
			return Run{}, fmt.Errorf("create run record: %w", err)
		}
	}
	return Run{}, errors.New("could not allocate a unique run ID")
}

func (s Store) Acquire(key string) (Lock, error) {
	if err := ensurePrivateDir(s.Root); err != nil {
		return Lock{}, err
	}
	if err := ensurePrivateDir(filepath.Join(s.Root, "locks")); err != nil {
		return Lock{}, err
	}
	if len(key) > 24 {
		key = key[:24]
	}
	path := filepath.Join(s.Root, "locks", key+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if errors.Is(err, os.ErrExist) {
		return Lock{}, errors.New("a CORA review is already running for this target; remove the stale lock if no process is active")
	}
	if err != nil {
		return Lock{}, err
	}
	_, writeErr := fmt.Fprintf(file, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return Lock{}, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return Lock{}, closeErr
	}
	return Lock{path: path}, nil
}

func (l Lock) Release() error {
	if l.path == "" {
		return nil
	}
	return os.Remove(l.path)
}

func (s Store) Finalize(run Run) error {
	if err := ensurePrivateDir(s.Root); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.Root, "latest"), []byte(run.ID+"\n"), privateFileMode)
}

func (s Store) Resolve(id string) (Run, error) {
	if id == "" || id == "latest" {
		runs, err := s.Runs()
		if err != nil {
			return Run{}, err
		}
		if len(runs) == 0 {
			return Run{}, errors.New("no CORA runs found")
		}
		return runs[0], nil
	}
	if id == "" || filepath.Base(id) != id {
		return Run{}, errors.New("invalid run ID")
	}
	path := filepath.Join(s.Root, "runs", id)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Run{}, fmt.Errorf("run not found: %s", id)
		}
		return Run{}, err
	}
	if !info.IsDir() {
		return Run{}, fmt.Errorf("run record is not a directory: %s", id)
	}
	return Run{ID: id, Path: path}, nil
}

func WriteHeartbeat(run Run, heartbeat model.Heartbeat) error {
	return WriteJSON(filepath.Join(run.Path, "heartbeat.json"), heartbeat)
}

func LoadHeartbeat(run Run) (model.Heartbeat, error) {
	var heartbeat model.Heartbeat
	err := ReadJSON(filepath.Join(run.Path, "heartbeat.json"), &heartbeat)
	return heartbeat, err
}

// AcquireProvider obtains one cross-process slot for a provider. The queue is
// user-global because provider quotas are shared across repositories.
func AcquireProvider(ctx context.Context, provider string, limit int, onWait func(time.Duration)) (ProviderLease, error) {
	if limit < 1 {
		return ProviderLease{}, errors.New("provider concurrency limit must be positive")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ProviderLease{}, fmt.Errorf("resolve provider queue directory: %w", err)
	}
	queueDir := filepath.Join(cacheDir, "cora", "provider-queue", safeComponent(provider))
	if err := ensurePrivateDir(queueDir); err != nil {
		return ProviderLease{}, err
	}
	started := time.Now()
	lastNotice := time.Time{}
	for {
		for slot := 0; slot < limit; slot++ {
			path := filepath.Join(queueDir, fmt.Sprintf("slot-%d.lock", slot))
			lease, acquired, err := tryProviderSlot(path)
			if err != nil {
				return ProviderLease{}, err
			}
			if acquired {
				return lease, nil
			}
		}
		if onWait != nil && (lastNotice.IsZero() || time.Since(lastNotice) >= 30*time.Second) {
			onWait(time.Since(started))
			lastNotice = time.Now()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ProviderLease{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryProviderSlot(path string) (ProviderLease, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err == nil {
		_, writeErr := fmt.Fprintf(file, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(path)
			return ProviderLease{}, false, errors.Join(writeErr, closeErr)
		}
		return ProviderLease{path: path}, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return ProviderLease{}, false, err
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil {
		return ProviderLease{}, false, nil
	}
	pid := lockPID(string(contents))
	info, statErr := os.Stat(path)
	staleByAge := statErr == nil && time.Since(info.ModTime()) > 24*time.Hour
	if (pid > 0 && !processAlive(pid)) || staleByAge {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return ProviderLease{}, false, removeErr
		}
	}
	return ProviderLease{}, false, nil
}

func (l ProviderLease) Release() error {
	if l.path == "" {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func lockPID(contents string) int {
	for _, line := range strings.Split(contents, "\n") {
		if value, found := strings.CutPrefix(line, "pid="); found {
			pid, _ := strconv.Atoi(value)
			return pid
		}
	}
	return 0
}

func safeComponent(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	if result.Len() == 0 {
		return "unknown"
	}
	return strings.Trim(result.String(), "-")
}

func (s Store) Runs() ([]Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			runs = append(runs, Run{ID: entry.Name(), Path: filepath.Join(s.Root, "runs", entry.Name())})
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID > runs[j].ID })
	return runs, nil
}

func WriteJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	contents = append(contents, '\n')
	return atomicWrite(path, contents, privateFileMode)
}

// WriteFile writes a private record artifact atomically.
func WriteFile(path string, contents []byte) error {
	return atomicWrite(path, contents, privateFileMode)
}

func ReadJSON(path string, value any) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(contents, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func AppendEvent(run Run, event any) error {
	path := filepath.Join(run.Path, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, privateFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(privateFileMode); err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	if err := json.NewEncoder(writer).Encode(event); err != nil {
		return err
	}
	return writer.Flush()
}

func LoadManifest(run Run) (model.Manifest, error) {
	var manifest model.Manifest
	err := ReadJSON(filepath.Join(run.Path, "manifest.json"), &manifest)
	return manifest, err
}

func LoadDecision(run Run) (model.Decision, error) {
	var decision model.Decision
	err := ReadJSON(filepath.Join(run.Path, "decision.json"), &decision)
	return decision, err
}

func atomicWrite(path string, contents []byte, mode os.FileMode) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".cora-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return err
	}
	return os.Chmod(path, privateDirMode)
}
