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
	"sync/atomic"
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
	path        string
	historyPath string
	started     time.Time
	provider    string
	runID       string
	reviewer    string
}

type ProviderQueueRequest struct {
	RunID    string
	Reviewer string
}

// ProviderQuotaError reports an unexpired provider cooldown shared by all
// CORA processes for the current user.
type ProviderQuotaError struct {
	Provider        string
	RetryAt         time.Time
	ProviderMessage string
}

func (e *ProviderQuotaError) Error() string {
	message := fmt.Sprintf("provider %s is quota-limited until %s", e.Provider, e.RetryAt.Format(time.RFC3339))
	if strings.TrimSpace(e.ProviderMessage) != "" {
		message += ": " + strings.TrimSpace(e.ProviderMessage)
	}
	return message
}

type providerTicket struct {
	PID        int       `json:"pid"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	RunID      string    `json:"run_id,omitempty"`
	Reviewer   string    `json:"reviewer,omitempty"`
}

type providerHistoryEntry struct {
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`
	RunID      string    `json:"run_id,omitempty"`
	Reviewer   string    `json:"reviewer,omitempty"`
}

type providerQuotaRecord struct {
	Provider        string    `json:"provider"`
	ProviderMessage string    `json:"provider_error"`
	RetryAt         time.Time `json:"retry_at"`
	RecordedAt      time.Time `json:"recorded_at"`
}

var providerTicketSequence atomic.Uint64
var providerQuotaSequence atomic.Uint64

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

func New(commonDir string) Store {
	return Store{Root: filepath.Join(commonDir, "cora")}
}

func (s Store) Create(started time.Time, headSHA string) (Run, error) {
	return s.createRecord("runs", started, headSHA)
}

func (s Store) CreateAutoFixLoop(started time.Time, headSHA string) (Run, error) {
	return s.createRecord("auto-fix", started, headSHA)
}

func (s Store) createRecord(collection string, started time.Time, headSHA string) (Run, error) {
	if err := ensurePrivateDir(s.Root); err != nil {
		return Run{}, fmt.Errorf("secure CORA record directory: %w", err)
	}
	root := filepath.Join(s.Root, collection)
	if err := ensurePrivateDir(root); err != nil {
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
		path := filepath.Join(root, id)
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
	started := time.Now()
	return AcquireProviderQueued(ctx, provider, limit, ProviderQueueRequest{}, func(status model.ProviderQueueStatus) {
		if onWait != nil {
			onWait(time.Since(started))
		}
	})
}

// RecordProviderQuota persists a provider-reported cooldown in the same
// user-global directory used by the provider concurrency queue.
func RecordProviderQuota(provider, providerMessage string, retryAt time.Time) error {
	if retryAt.IsZero() {
		return errors.New("provider quota retry time must be set")
	}
	queueDir, err := providerQueueDirectory(provider)
	if err != nil {
		return err
	}
	record := providerQuotaRecord{
		Provider: provider, ProviderMessage: strings.TrimSpace(providerMessage),
		RetryAt: retryAt, RecordedAt: time.Now().UTC(),
	}
	name := fmt.Sprintf("quota-%020d-%d-%06d.json", record.RecordedAt.UnixNano(), os.Getpid(), providerQuotaSequence.Add(1))
	if err := WriteJSON(filepath.Join(queueDir, name), record); err != nil {
		return fmt.Errorf("record provider quota: %w", err)
	}
	return nil
}

// AcquireProviderQueued obtains a provider slot in FIFO order and reports a
// best-effort position and ETA based on recent execution durations.
func AcquireProviderQueued(ctx context.Context, provider string, limit int, request ProviderQueueRequest, onWait func(model.ProviderQueueStatus)) (ProviderLease, error) {
	if limit < 1 {
		return ProviderLease{}, errors.New("provider concurrency limit must be positive")
	}
	queueDir, err := providerQueueDirectory(provider)
	if err != nil {
		return ProviderLease{}, err
	}
	if err := providerQuotaGate(queueDir, provider, time.Now()); err != nil {
		return ProviderLease{}, err
	}
	started := time.Now().UTC()
	ticket := providerTicket{PID: os.Getpid(), EnqueuedAt: started, RunID: request.RunID, Reviewer: request.Reviewer}
	ticketName := fmt.Sprintf("ticket-%020d-%d-%06d.json", started.UnixNano(), os.Getpid(), providerTicketSequence.Add(1))
	ticketPath := filepath.Join(queueDir, ticketName)
	if err := WriteJSON(ticketPath, ticket); err != nil {
		return ProviderLease{}, fmt.Errorf("create provider queue ticket: %w", err)
	}
	defer os.Remove(ticketPath)
	historyPath := filepath.Join(queueDir, "history.jsonl")
	lastNotice := time.Time{}
	var estimatedAt *time.Time
	for {
		if err := providerQuotaGate(queueDir, provider, time.Now()); err != nil {
			return ProviderLease{}, err
		}
		tickets, err := liveProviderTickets(queueDir)
		if err != nil {
			return ProviderLease{}, err
		}
		position := ticketPosition(tickets, ticketName)
		active, err := activeProviderSlots(queueDir, limit)
		if err != nil {
			return ProviderLease{}, err
		}
		available := limit - active
		if position > 0 && position <= available {
			for slot := 0; slot < limit; slot++ {
				path := filepath.Join(queueDir, fmt.Sprintf("slot-%d.lock", slot))
				lease, acquired, err := tryProviderSlot(path, provider, request)
				if err != nil {
					return ProviderLease{}, err
				}
				if acquired {
					_ = os.Remove(ticketPath)
					lease.historyPath = historyPath
					return lease, nil
				}
			}
		}
		if onWait != nil && (lastNotice.IsZero() || time.Since(lastNotice) >= 30*time.Second) {
			status := model.ProviderQueueStatus{
				Provider: provider, Position: max(position, 1), Ahead: max(position-1, 0),
				Active: active, Limit: limit, WaitMS: time.Since(started).Milliseconds(),
			}
			if estimate := providerQueueETA(historyPath, status.Position, limit); estimate > 0 {
				estimatedAt = fixedProviderETA(estimatedAt, time.Now().UTC(), estimate)
				eta := *estimatedAt
				status.ETAAt = &eta
			}
			onWait(status)
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

func providerQueueDirectory(provider string) (string, error) {
	queueRoot := strings.TrimSpace(os.Getenv("CORA_PROVIDER_QUEUE_DIR"))
	if queueRoot == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve provider queue directory: %w", err)
		}
		queueRoot = filepath.Join(cacheDir, "cora", "provider-queue")
	}
	queueDir := filepath.Join(queueRoot, safeComponent(provider))
	if err := ensurePrivateDir(queueDir); err != nil {
		return "", err
	}
	return queueDir, nil
}

func providerQuotaGate(queueDir, provider string, now time.Time) error {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return fmt.Errorf("read provider quota directory: %w", err)
	}
	var active *providerQuotaRecord
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (name != "quota.json" && (!strings.HasPrefix(name, "quota-") || !strings.HasSuffix(name, ".json"))) {
			continue
		}
		path := filepath.Join(queueDir, name)
		var candidate providerQuotaRecord
		if err := ReadJSON(path, &candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read provider quota: %w", err)
		}
		if !candidate.RetryAt.After(now) {
			if name != "quota.json" {
				_ = os.Remove(path)
			}
			continue
		}
		if active == nil || candidate.RetryAt.After(active.RetryAt) || (candidate.RetryAt.Equal(active.RetryAt) && candidate.RecordedAt.After(active.RecordedAt)) {
			copy := candidate
			active = &copy
		}
	}
	if active == nil {
		return nil
	}
	recordedProvider := active.Provider
	if strings.TrimSpace(recordedProvider) == "" {
		recordedProvider = provider
	}
	return &ProviderQuotaError{
		Provider: recordedProvider, RetryAt: active.RetryAt,
		ProviderMessage: active.ProviderMessage,
	}
}

// fixedProviderETA chooses one absolute deadline for a queue wait. Subsequent
// notices keep that deadline, so every display derives a countdown from the
// same instant instead of repeatedly adding an estimate to the current time.
func fixedProviderETA(current *time.Time, observedAt time.Time, estimate time.Duration) *time.Time {
	if current != nil {
		value := *current
		return &value
	}
	value := observedAt.Add(estimate)
	return &value
}

func tryProviderSlot(path, provider string, request ProviderQueueRequest) (ProviderLease, bool, error) {
	started := time.Now().UTC()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err == nil {
		_, writeErr := fmt.Fprintf(file, "pid=%d\nstarted=%s\nrun_id=%s\nreviewer=%s\n", os.Getpid(), started.Format(time.RFC3339Nano), request.RunID, request.Reviewer)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(path)
			return ProviderLease{}, false, errors.Join(writeErr, closeErr)
		}
		return ProviderLease{path: path, started: started, provider: provider, runID: request.RunID, reviewer: request.Reviewer}, true, nil
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

func liveProviderTickets(queueDir string) ([]string, error) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil, err
	}
	var tickets []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "ticket-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(queueDir, entry.Name())
		var ticket providerTicket
		readErr := ReadJSON(path, &ticket)
		info, statErr := entry.Info()
		staleByAge := statErr == nil && time.Since(info.ModTime()) > 24*time.Hour
		if readErr != nil || staleByAge || (ticket.PID > 0 && !processAlive(ticket.PID)) {
			_ = os.Remove(path)
			continue
		}
		tickets = append(tickets, entry.Name())
	}
	sort.Strings(tickets)
	return tickets, nil
}

func ticketPosition(tickets []string, name string) int {
	for index, ticket := range tickets {
		if ticket == name {
			return index + 1
		}
	}
	return 0
}

func activeProviderSlots(queueDir string, limit int) (int, error) {
	active := 0
	for slot := 0; slot < limit; slot++ {
		path := filepath.Join(queueDir, fmt.Sprintf("slot-%d.lock", slot))
		contents, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		info, statErr := os.Stat(path)
		staleByAge := statErr == nil && time.Since(info.ModTime()) > 24*time.Hour
		pid := lockPID(string(contents))
		if staleByAge || (pid > 0 && !processAlive(pid)) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			continue
		}
		active++
	}
	return active, nil
}

func providerQueueETA(historyPath string, position, limit int) time.Duration {
	file, err := os.Open(historyPath)
	if err != nil {
		return 0
	}
	defer file.Close()
	var durations []time.Duration
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry providerHistoryEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.DurationMS > 0 {
			durations = append(durations, time.Duration(entry.DurationMS)*time.Millisecond)
			if len(durations) > 20 {
				durations = durations[1:]
			}
		}
	}
	if len(durations) == 0 {
		return 0
	}
	var total time.Duration
	for _, duration := range durations {
		total += duration
	}
	average := total / time.Duration(len(durations))
	waves := (max(position, 1) + limit - 1) / limit
	return time.Duration(waves) * average
}

func (l ProviderLease) Release() error {
	if l.path == "" {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if l.historyPath != "" && !l.started.IsZero() {
		entry := providerHistoryEntry{FinishedAt: time.Now().UTC(), DurationMS: time.Since(l.started).Milliseconds(), RunID: l.runID, Reviewer: l.reviewer}
		contents, err := json.Marshal(entry)
		if err == nil {
			file, openErr := os.OpenFile(l.historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, privateFileMode)
			if openErr == nil {
				_, writeErr := file.Write(append(contents, '\n'))
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					return errors.Join(writeErr, closeErr)
				}
			}
		}
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
