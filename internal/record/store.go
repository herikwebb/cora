package record

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// ReviewerLineage is the reusable reviewer state recovered from an exact-diff
// retry chain. Security reviews are kept separate because the orchestrator
// executes them as a targeted pass rather than as ordinary consensus input.
type ReviewerLineage struct {
	Reviewers             []model.ReviewerResult
	SecurityReviews       []model.ReviewerResult
	LatestReviewers       []model.ReviewerResult
	LatestSecurityReviews []model.ReviewerResult
}

// ApprovedBaseline is a completed full-diff approval that can anchor a
// narrower auto-fix delta review. Its decision and canonical patch remain
// bound to the immutable exact target recorded by the original run.
type ApprovedBaseline struct {
	Run      Run
	Decision model.Decision
	Manifest model.Manifest
	Patch    []byte
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

// ResolveAutoFix resolves a parent loop record without falling back to the
// ordinary review collection. It is the entry point used by an explicit
// resume command after a retryable quota pause.
func (s Store) ResolveAutoFix(id string) (Run, error) {
	if id == "" || filepath.Base(id) != id {
		return Run{}, errors.New("invalid auto-fix loop ID")
	}
	path := filepath.Join(s.Root, "auto-fix", id)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Run{}, fmt.Errorf("auto-fix loop not found: %s", id)
		}
		return Run{}, err
	}
	if !info.IsDir() {
		return Run{}, fmt.Errorf("auto-fix loop record is not a directory: %s", id)
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
	// The median keeps a single machine sleep or unusually slow provider call
	// from making every subsequent queue estimate look hours long.
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	if len(durations)%2 == 0 {
		median = (durations[len(durations)/2-1] + median) / 2
	}
	waves := (max(position, 1) + limit - 1) / limit
	return time.Duration(waves) * median
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
	return s.records("runs")
}

// AutoFixLoops returns parent loop records newest first. Review child runs are
// kept in Runs; callers can combine the two views without relying on `latest`.
func (s Store) AutoFixLoops() ([]Run, error) {
	return s.records("auto-fix")
}

func (s Store) records(collection string) ([]Run, error) {
	root := filepath.Join(s.Root, collection)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			runs = append(runs, Run{ID: entry.Name(), Path: filepath.Join(root, entry.Name())})
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID > runs[j].ID })
	return runs, nil
}

// LatestExactApproval returns the newest unanimous full-scope approval for
// exactly target. Delta approvals are deliberately ineligible: they can only
// be composed with the full approval they reference and must never become a
// replacement full-diff baseline by themselves.
func (s Store) LatestExactApproval(target model.Target) (ApprovedBaseline, bool, error) {
	runs, err := s.Runs()
	if err != nil {
		return ApprovedBaseline{}, false, err
	}
	for _, run := range runs {
		decision, decisionErr := LoadDecision(run)
		if errors.Is(decisionErr, os.ErrNotExist) {
			continue
		}
		if decisionErr != nil {
			return ApprovedBaseline{}, false, fmt.Errorf("load approval candidate %s: %w", run.ID, decisionErr)
		}
		if decision.State != model.StateApproved || decision.BaseSHA != target.BaseSHA || decision.HeadSHA != target.HeadSHA || decision.DiffHash != target.DiffHash {
			continue
		}
		baseline, loadErr := LoadApprovedBaseline(run)
		if loadErr != nil {
			if errors.Is(loadErr, os.ErrNotExist) || errors.Is(loadErr, ErrNotApprovedBaseline) {
				continue
			}
			return ApprovedBaseline{}, false, fmt.Errorf("load approval baseline %s: %w", run.ID, loadErr)
		}
		if sameExactTarget(baseline.Manifest.Target, target) &&
			baseline.Decision.BaseSHA == target.BaseSHA && baseline.Decision.HeadSHA == target.HeadSHA && baseline.Decision.DiffHash == target.DiffHash {
			return baseline, true, nil
		}
	}
	return ApprovedBaseline{}, false, nil
}

var ErrNotApprovedBaseline = errors.New("run is not an eligible approved baseline")

// LoadApprovedBaseline validates the record rather than trusting its decision
// label. The canonical patch, manifest target, completed reviewer consensus,
// and checks must all agree before auto-fix may preserve the approval.
func LoadApprovedBaseline(run Run) (ApprovedBaseline, error) {
	decision, err := LoadDecision(run)
	if err != nil {
		return ApprovedBaseline{}, err
	}
	if decision.State != model.StateApproved {
		return ApprovedBaseline{}, ErrNotApprovedBaseline
	}
	manifest, err := LoadManifest(run)
	if err != nil {
		return ApprovedBaseline{}, err
	}
	if manifest.ReviewScope == "approved-baseline-delta" ||
		!sameExactTarget(manifest.Target, model.Target{BaseSHA: decision.BaseSHA, HeadSHA: decision.HeadSHA, DiffHash: decision.DiffHash}) ||
		!unanimousCompletedApproval(decision, append(append([]model.ReviewerResult(nil), manifest.Reviewers...), manifest.SecurityReviews...)) || !allChecksPassed(manifest.Checks) {
		return ApprovedBaseline{}, ErrNotApprovedBaseline
	}
	patch, err := os.ReadFile(filepath.Join(run.Path, "target.diff"))
	if err != nil {
		return ApprovedBaseline{}, err
	}
	sum := sha256.Sum256(patch)
	if hex.EncodeToString(sum[:]) != decision.DiffHash {
		return ApprovedBaseline{}, fmt.Errorf("%w: canonical patch hash does not match decision", ErrNotApprovedBaseline)
	}
	return ApprovedBaseline{Run: run, Decision: decision, Manifest: manifest, Patch: patch}, nil
}

func sameExactTarget(left, right model.Target) bool {
	return left.BaseSHA == right.BaseSHA && left.HeadSHA == right.HeadSHA && left.DiffHash == right.DiffHash
}

func unanimousCompletedApproval(decision model.Decision, results []model.ReviewerResult) bool {
	if len(decision.Reviewers) == 0 {
		return false
	}
	completed := make(map[string]bool, len(results))
	for _, result := range results {
		if result.Status == "completed" && result.Report != nil && result.Report.ContextComplete && result.Report.Verdict == "approve" {
			completed[result.Reviewer] = true
		}
	}
	for reviewer, result := range decision.Reviewers {
		if result != "approve" || !completed[reviewer] {
			return false
		}
	}
	return true
}

func allChecksPassed(checks []model.CheckResult) bool {
	for _, check := range checks {
		if check.Status != "passed" {
			return false
		}
	}
	return true
}

// ExactDiffReviewerLineage walks a retry's parent chain and returns the newest
// completed result for every reviewer. A newer failed retry does not erase an
// earlier completed review of the identical immutable diff. When a reviewer
// has never completed, its newest attempt is retained so quota and retry
// metadata remain available. Any target mismatch or cycle makes the lineage
// unusable rather than allowing approval evidence to cross diff boundaries.
func (s Store) ExactDiffReviewerLineage(run Run, target model.Target, repositoryIdentity string) (ReviewerLineage, error) {
	seenRuns := make(map[string]bool)
	ordinary := newReviewerLineageAccumulator()
	security := newReviewerLineageAccumulator()
	for run.ID != "" {
		if seenRuns[run.ID] {
			return ReviewerLineage{}, fmt.Errorf("retry lineage contains a cycle at run %s", run.ID)
		}
		seenRuns[run.ID] = true
		manifest, err := LoadManifest(run)
		if err != nil {
			return ReviewerLineage{}, fmt.Errorf("load retry lineage manifest %s: %w", run.ID, err)
		}
		if !sameExactTarget(manifest.Target, target) {
			return ReviewerLineage{}, fmt.Errorf("retry lineage run %s targets a different diff", run.ID)
		}
		if repositoryIdentity != "" && manifest.RepositoryIdentity != repositoryIdentity {
			return ReviewerLineage{}, fmt.Errorf("retry lineage run %s belongs to a different repository identity", run.ID)
		}
		ordinary.add(manifest.Reviewers, run.ID)
		security.add(manifest.SecurityReviews, run.ID)
		if manifest.ParentRunID == "" {
			break
		}
		run, err = s.Resolve(manifest.ParentRunID)
		if err != nil {
			return ReviewerLineage{}, fmt.Errorf("resolve retry lineage parent %s: %w", manifest.ParentRunID, err)
		}
	}
	return ReviewerLineage{
		Reviewers: ordinary.results(), SecurityReviews: security.results(),
		LatestReviewers: ordinary.latestResults(), LatestSecurityReviews: security.latestResults(),
	}, nil
}

type reviewerLineageAccumulator struct {
	latest    map[string]model.ReviewerResult
	completed map[string]model.ReviewerResult
}

func newReviewerLineageAccumulator() *reviewerLineageAccumulator {
	return &reviewerLineageAccumulator{
		latest: make(map[string]model.ReviewerResult), completed: make(map[string]model.ReviewerResult),
	}
}

func (a *reviewerLineageAccumulator) add(results []model.ReviewerResult, runID string) {
	for _, result := range results {
		name := strings.TrimSpace(result.Reviewer)
		if name == "" {
			continue
		}
		if _, found := a.latest[name]; !found {
			if result.ReusedFromRunID == "" {
				result.ReusedFromRunID = runID
			}
			a.latest[name] = result
		}
		if _, found := a.completed[name]; !found && result.Status == "completed" && result.Report != nil {
			if result.ReusedFromRunID == "" {
				result.ReusedFromRunID = runID
			}
			a.completed[name] = result
		}
	}
}

func (a *reviewerLineageAccumulator) results() []model.ReviewerResult {
	names := make([]string, 0, len(a.latest))
	for name := range a.latest {
		names = append(names, name)
	}
	sort.Strings(names)
	results := make([]model.ReviewerResult, 0, len(names))
	for _, name := range names {
		result, found := a.completed[name]
		if !found {
			result = a.latest[name]
		}
		results = append(results, result)
	}
	return results
}

func (a *reviewerLineageAccumulator) latestResults() []model.ReviewerResult {
	names := make([]string, 0, len(a.latest))
	for name := range a.latest {
		names = append(names, name)
	}
	sort.Strings(names)
	results := make([]model.ReviewerResult, 0, len(names))
	for _, name := range names {
		results = append(results, a.latest[name])
	}
	return results
}

// UnresolvedFindings returns the latest known disposition of findings from
// completed reviewer results for the same exact target. A finding remains
// carried when a newer run omits it; only an explicit rejected-finding record
// backed by a completed reviewer retires it. Partial-only recovery evidence
// remains in its original audit record but is never promoted to durable truth.
func (s Store) UnresolvedFindings(target model.Target) ([]model.ConsolidatedFinding, error) {
	runs, err := s.Runs()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var findings []model.ConsolidatedFinding
	for _, run := range runs {
		decision, loadErr := LoadDecision(run)
		if errors.Is(loadErr, os.ErrNotExist) {
			continue
		}
		if loadErr != nil {
			return nil, fmt.Errorf("load prior decision %s: %w", run.ID, loadErr)
		}
		if decision.DiffHash != target.DiffHash || decision.BaseSHA != target.BaseSHA || decision.HeadSHA != target.HeadSHA {
			continue
		}
		for _, rejected := range decision.RejectedFindings {
			if !findingBackedByCompletedReviewers(decision, rejected) {
				continue
			}
			for _, key := range findingRecordKeys(rejected) {
				seen[key] = true
			}
		}
		carryForward := decision.CarryForwardFindings
		legacyCarryPolicy := carryForward == nil
		if legacyCarryPolicy {
			carryForward = decision.Findings
		}
		for _, finding := range carryForward {
			if legacyCarryPolicy && !findingBackedByCompletedReviewers(decision, finding) {
				continue
			}
			keys := findingRecordKeys(finding)
			alreadySeen := false
			for _, key := range keys {
				if seen[key] {
					alreadySeen = true
					break
				}
			}
			for _, key := range keys {
				seen[key] = true
			}
			if alreadySeen {
				continue
			}
			finding.CarriedFromRunIDs = appendUnique(finding.CarriedFromRunIDs, run.ID)
			findings = append(findings, finding)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].ID < findings[j].ID
	})
	return findings, nil
}

func findingBackedByCompletedReviewers(decision model.Decision, finding model.ConsolidatedFinding) bool {
	if len(finding.Reviewers) == 0 {
		return len(decision.Reviewers) == 0 && (decision.State == model.StateApproved || decision.State == model.StateChangesRequested)
	}
	for _, reviewer := range finding.Reviewers {
		switch decision.Reviewers[reviewer] {
		case "approve", "request_changes":
			continue
		}
		completedCrossExamination := false
		for _, examination := range decision.CrossExaminations {
			if examination.Reviewer == reviewer && examination.Status == "completed" {
				completedCrossExamination = true
				break
			}
		}
		if !completedCrossExamination {
			return false
		}
	}
	return true
}

func findingRecordKeys(finding model.ConsolidatedFinding) []string {
	keys := make([]string, 0, len(finding.HistoricalFindingIDs)+1)
	if strings.TrimSpace(finding.ID) != "" {
		keys = append(keys, finding.ID)
	}
	for _, historicalID := range finding.HistoricalFindingIDs {
		if strings.TrimSpace(historicalID) != "" {
			keys = appendUnique(keys, historicalID)
		}
	}
	return keys
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
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

func LoadAutoFixLoop(run Run) (model.AutoFixLoop, error) {
	var loop model.AutoFixLoop
	err := ReadJSON(filepath.Join(run.Path, "manifest.json"), &loop)
	return loop, err
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
