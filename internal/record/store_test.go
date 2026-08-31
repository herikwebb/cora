package record

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
)

func TestStoreLifecycleAndLock(t *testing.T) {
	store := New(t.TempDir())
	lock, err := store.Acquire("abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("abcdef"); err == nil {
		t.Fatal("expected duplicate lock to fail")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	run, err := store.Create(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	decision := model.Decision{SchemaVersion: "1", RunID: run.ID, State: model.StateApproved}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(run); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Resolve("latest")
	if err != nil || latest.ID != run.ID {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	loaded, err := LoadDecision(latest)
	if err != nil || loaded.State != model.StateApproved {
		t.Fatalf("decision = %#v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "latest")); err != nil {
		t.Fatal(err)
	}
	loop, err := store.CreateAutoFixLoop(time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(loop.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 || filepath.Base(filepath.Dir(loop.Path)) != "auto-fix" {
		t.Fatalf("auto-fix record = %s, mode=%v", loop.Path, info.Mode().Perm())
	}
}

func TestAutoFixLoopsReturnsParentRecordsNewestFirst(t *testing.T) {
	store := New(t.TempDir())
	first, err := store.CreateAutoFixLoop(time.Unix(1, 0), "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAutoFixLoop(time.Unix(2, 0), "second")
	if err != nil {
		t.Fatal(err)
	}
	loops, err := store.AutoFixLoops()
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 2 || loops[0].ID != second.ID || loops[1].ID != first.ID {
		t.Fatalf("auto-fix loops = %#v", loops)
	}
}

func TestExactDiffReviewerLineagePreservesNewestCompletedResults(t *testing.T) {
	store := New(t.TempDir())
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff"}
	completedReport := func(verdict string) *model.ReviewReport {
		return &model.ReviewReport{SchemaVersion: model.SchemaVersion, Verdict: verdict, ContextComplete: true}
	}
	first, err := store.Create(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(first.Path, "manifest.json"), model.Manifest{
		RunID: first.ID, Target: target, RepositoryIdentity: "repo-id",
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Report: completedReport("approve"), Attempt: 1},
			{Reviewer: "claude", Status: "incomplete", Attempt: 1},
		},
		SecurityReviews: []model.ReviewerResult{{Reviewer: "claude-security", Status: "completed", Report: completedReport("approve"), Model: "fable-old", Effort: "high", Attempt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(second.Path, "manifest.json"), model.Manifest{
		RunID: second.ID, ParentRunID: first.ID, Target: target, RepositoryIdentity: "repo-id",
		Reviewers: []model.ReviewerResult{
			{Reviewer: "claude", Status: "completed", Report: completedReport("request_changes"), Attempt: 2},
			{Reviewer: "codex", Status: "completed", Report: completedReport("approve"), Attempt: 1, ReusedFromRunID: first.ID},
		},
		SecurityReviews: []model.ReviewerResult{{Reviewer: "claude-security", Status: "completed", Report: completedReport("approve"), Model: "fable-new", Effort: "high", Attempt: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	third, err := store.Create(time.Date(2026, 1, 2, 3, 4, 7, 0, time.UTC), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(third.Path, "manifest.json"), model.Manifest{
		RunID: third.ID, ParentRunID: second.ID, Target: target, RepositoryIdentity: "repo-id",
		Reviewers:       []model.ReviewerResult{{Reviewer: "claude", Status: "incomplete", FailureKind: "quota", Attempt: 3}},
		SecurityReviews: []model.ReviewerResult{{Reviewer: "claude-security", Status: "incomplete", FailureKind: "quota", Attempt: 3}},
	}); err != nil {
		t.Fatal(err)
	}

	lineage, err := store.ExactDiffReviewerLineage(third, target, "repo-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(lineage.Reviewers) != 2 || lineage.Reviewers[0].Reviewer != "claude" || lineage.Reviewers[0].Status != "completed" || lineage.Reviewers[0].Attempt != 2 || lineage.Reviewers[0].ReusedFromRunID != second.ID {
		t.Fatalf("ordinary lineage = %#v", lineage.Reviewers)
	}
	if lineage.Reviewers[1].Reviewer != "codex" || lineage.Reviewers[1].Status != "completed" || lineage.Reviewers[1].ReusedFromRunID != first.ID {
		t.Fatalf("ordinary lineage = %#v", lineage.Reviewers)
	}
	if len(lineage.SecurityReviews) != 1 || lineage.SecurityReviews[0].Status != "completed" || lineage.SecurityReviews[0].Model != "fable-new" || lineage.SecurityReviews[0].ReusedFromRunID != second.ID {
		t.Fatalf("security lineage = %#v", lineage.SecurityReviews)
	}
	if len(lineage.LatestReviewers) != 2 || lineage.LatestReviewers[0].Reviewer != "claude" || lineage.LatestReviewers[0].Status != "incomplete" || lineage.LatestReviewers[0].Attempt != 3 {
		t.Fatalf("latest ordinary lineage = %#v", lineage.LatestReviewers)
	}
	if len(lineage.LatestSecurityReviews) != 1 || lineage.LatestSecurityReviews[0].Status != "incomplete" || lineage.LatestSecurityReviews[0].Attempt != 3 {
		t.Fatalf("latest security lineage = %#v", lineage.LatestSecurityReviews)
	}
}

func TestExactDiffReviewerLineageRejectsMismatchedAncestor(t *testing.T) {
	store := New(t.TempDir())
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff"}
	parent, err := store.Create(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(parent.Path, "manifest.json"), model.Manifest{RunID: parent.ID, Target: model.Target{BaseSHA: "other", HeadSHA: "head", DiffHash: "diff"}}); err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(child.Path, "manifest.json"), model.Manifest{RunID: child.ID, ParentRunID: parent.ID, Target: target}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ExactDiffReviewerLineage(child, target, ""); err == nil || !strings.Contains(err.Error(), "different diff") {
		t.Fatalf("mismatched lineage error = %v", err)
	}
}

func TestProviderLeaseQueueIsFIFOAndReportsETA(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	first, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "first"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	held, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "held"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type acquired struct {
		name  string
		lease ProviderLease
		err   error
	}
	results := make(chan acquired, 2)
	start := func(name string) {
		go func() {
			lease, acquireErr := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: name}, nil)
			results <- acquired{name: name, lease: lease, err: acquireErr}
		}()
	}
	start("second")
	waitForTicketCount(t, filepath.Join(root, safeComponent(provider)), 1)
	start("third")
	waitForTicketCount(t, filepath.Join(root, safeComponent(provider)), 2)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	second := <-results
	if second.err != nil || second.name != "second" {
		t.Fatalf("first queued acquisition = %#v", second)
	}
	select {
	case unexpected := <-results:
		t.Fatalf("third acquired before second released: %#v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
	if err := second.lease.Release(); err != nil {
		t.Fatal(err)
	}
	third := <-results
	if third.err != nil || third.name != "third" {
		t.Fatalf("second queued acquisition = %#v", third)
	}
	if err := third.lease.Release(); err != nil {
		t.Fatal(err)
	}

	active, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "active"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	statusChannel := make(chan model.ProviderQueueStatus, 1)
	go func() {
		_, _ = AcquireProviderQueued(ctx, provider, 1, ProviderQueueRequest{Reviewer: "eta"}, func(status model.ProviderQueueStatus) {
			select {
			case statusChannel <- status:
			default:
			}
		})
	}()
	status := <-statusChannel
	cancel()
	_ = active.Release()
	if status.Position != 1 || status.Active != 1 || status.ETAAt == nil {
		t.Fatalf("queue status = %#v", status)
	}
}

func TestFixedProviderETACountsDownFromOneDeadline(t *testing.T) {
	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	initial := fixedProviderETA(nil, started, 10*time.Minute)
	later := fixedProviderETA(initial, started.Add(30*time.Second), 15*time.Minute)
	if !later.Equal(*initial) {
		t.Fatalf("ETA slid forward from %s to %s", initial, later)
	}
	if remaining := later.Sub(started.Add(30 * time.Second)); remaining != 9*time.Minute+30*time.Second {
		t.Fatalf("ETA did not count down from its fixed deadline: %s", remaining)
	}
}

func TestProviderQueueETAUsesMedianToIgnoreSuspendOutlier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, duration := range []int64{60_000, 65_000, 8 * 60 * 60 * 1000} {
		if _, err := fmt.Fprintf(file, `{"duration_ms":%d}`+"\n", duration); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := providerQueueETA(path, 1, 1); got != 65*time.Second {
		t.Fatalf("queue ETA with suspend outlier = %s", got)
	}
}

func TestProviderQuotaPersistsAcrossProcessesAndBlocksAcquire(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	retryAt := time.Now().UTC().Add(time.Hour).Round(0)
	providerMessage := "session limit reached"

	command := exec.Command(os.Args[0], "-test.run=^TestProviderQuotaHelperProcess$")
	command.Env = append(os.Environ(),
		"CORA_RECORD_QUOTA_HELPER=1",
		"CORA_RECORD_QUOTA_PROVIDER="+provider,
		"CORA_RECORD_QUOTA_MESSAGE="+providerMessage,
		"CORA_RECORD_QUOTA_RETRY_AT="+retryAt.Format(time.RFC3339Nano),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("record quota in helper process: %v\n%s", err, output)
	}

	_, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "blocked"}, nil)
	var quotaErr *ProviderQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("acquire error = %v, want ProviderQuotaError", err)
	}
	if quotaErr.Provider != provider || !quotaErr.RetryAt.Equal(retryAt) || quotaErr.ProviderMessage != providerMessage {
		t.Fatalf("quota error = %#v", quotaErr)
	}

	quotaFiles, globErr := filepath.Glob(filepath.Join(root, safeComponent(provider), "quota-*.json"))
	if globErr != nil || len(quotaFiles) != 1 {
		t.Fatalf("quota files = %v, err=%v", quotaFiles, globErr)
	}
	quotaPath := quotaFiles[0]
	info, statErr := os.Stat(quotaPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("quota mode = %#o, want 0600", info.Mode().Perm())
	}
	directoryInfo, statErr := os.Stat(filepath.Dir(quotaPath))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("provider queue directory mode = %#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func TestProviderQuotaHelperProcess(t *testing.T) {
	if os.Getenv("CORA_RECORD_QUOTA_HELPER") != "1" {
		return
	}
	retryAt, err := time.Parse(time.RFC3339Nano, os.Getenv("CORA_RECORD_QUOTA_RETRY_AT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := RecordProviderQuota(os.Getenv("CORA_RECORD_QUOTA_PROVIDER"), os.Getenv("CORA_RECORD_QUOTA_MESSAGE"), retryAt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestExpiredProviderQuotaDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if err := RecordProviderQuota(provider, "old quota error", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "allowed"}, nil)
	if err != nil {
		t.Fatalf("expired quota blocked acquisition: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderQuotaCannotBeWeakenedByOlderOrExpiredObservation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	later := time.Now().UTC().Add(2 * time.Hour).Round(0)
	if err := RecordProviderQuota(provider, "later reset", later); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderQuota(provider, "stale earlier reset", later.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderQuota(provider, "already expired", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "blocked"}, nil)
	var quotaErr *ProviderQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("acquire error = %v, want ProviderQuotaError", err)
	}
	if !quotaErr.RetryAt.Equal(later) || quotaErr.ProviderMessage != "later reset" {
		t.Fatalf("active quota was weakened: %#v", quotaErr)
	}
}

func TestProviderQuotaRecordedWhileQueuedStopsWaiter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", root)
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	held, err := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "held"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	result := make(chan error, 1)
	go func() {
		_, acquireErr := AcquireProviderQueued(context.Background(), provider, 1, ProviderQueueRequest{Reviewer: "queued"}, nil)
		result <- acquireErr
	}()
	waitForTicketCount(t, filepath.Join(root, safeComponent(provider)), 1)
	if err := RecordProviderQuota(provider, "quota discovered by another process", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var quotaErr *ProviderQuotaError
		if !errors.As(err, &quotaErr) {
			t.Fatalf("queued acquire error = %v, want ProviderQuotaError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued acquisition did not observe provider quota")
	}
}

func waitForTicketCount(t *testing.T, directory string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(directory)
		count := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ticket-") {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provider ticket count did not reach %d", want)
}

func TestLatestUsesRunStartOrderInsteadOfCompletionOrder(t *testing.T) {
	store := New(t.TempDir())
	first, err := store.Create(time.Date(2026, 8, 25, 12, 0, 0, 1, time.UTC), strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(time.Date(2026, 8, 25, 12, 0, 0, 2, time.UTC), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(second); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(first); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Resolve("latest")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest = %s, want newest-started %s", latest.ID, second.ID)
	}
}

func TestExactDiffReviewerLineageRejectsMismatchedRepositoryIdentity(t *testing.T) {
	store := New(t.TempDir())
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff"}
	parent, err := store.Create(time.Unix(1, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(parent.Path, "manifest.json"), model.Manifest{RunID: parent.ID, Target: target, RepositoryIdentity: "old-remote"}); err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(time.Unix(2, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(child.Path, "manifest.json"), model.Manifest{RunID: child.ID, ParentRunID: parent.ID, Target: target, RepositoryIdentity: "current-remote"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExactDiffReviewerLineage(child, target, "current-remote"); err == nil || !strings.Contains(err.Error(), "repository identity") {
		t.Fatalf("mismatched repository lineage error = %v", err)
	}
}

func TestUnresolvedFindingsSurviveOmissionUntilExplicitlyRejected(t *testing.T) {
	store := New(t.TempDir())
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff"}
	finding := model.ConsolidatedFinding{
		ID: "finding-1", Severity: "major", File: "runner.go", Line: 42,
		Claim: "Recurring runs can overlap", Reviewers: []string{"codex"}, SourceIDs: []string{"codex-1"},
	}
	first, err := store.Create(time.Unix(1, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(first.Path, "decision.json"), model.Decision{
		RunID: first.ID, State: model.StateChangesRequested, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "request_changes"},
		Findings:  []model.ConsolidatedFinding{finding},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(time.Unix(2, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(second.Path, "decision.json"), model.Decision{
		RunID: second.ID, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
	}); err != nil {
		t.Fatal(err)
	}

	carried, err := store.UnresolvedFindings(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 1 || carried[0].ID != finding.ID || len(carried[0].CarriedFromRunIDs) != 1 || carried[0].CarriedFromRunIDs[0] != first.ID {
		t.Fatalf("carried findings = %#v", carried)
	}

	third, err := store.Create(time.Unix(3, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	rejectedAlias := finding
	rejectedAlias.ID = "newly-consolidated-id"
	rejectedAlias.HistoricalFindingIDs = []string{finding.ID}
	if err := WriteJSON(filepath.Join(third.Path, "decision.json"), model.Decision{
		RunID: third.ID, State: model.StateApproved, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers:        map[string]string{"codex": "approve"},
		RejectedFindings: []model.ConsolidatedFinding{rejectedAlias},
	}); err != nil {
		t.Fatal(err)
	}
	carried, err = store.UnresolvedFindings(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 0 {
		t.Fatalf("explicitly rejected finding was still carried: %#v", carried)
	}
}

func TestUnresolvedFindingsDoNotPromotePartialOnlyRecoveryEvidence(t *testing.T) {
	store := New(t.TempDir())
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: "diff"}
	run, err := store.Create(time.Unix(1, 0), target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), model.Decision{
		RunID: run.ID, State: model.StateIncomplete, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"claude": "partial"},
		Findings: []model.ConsolidatedFinding{{
			ID: "checkpoint-only", Severity: "major", File: "app.go", Line: 12,
			Claim: "A checkpoint reported an issue before timeout.", Reviewers: []string{"claude"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	carried, err := store.UnresolvedFindings(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 0 {
		t.Fatalf("partial-only evidence became an authoritative carried finding: %#v", carried)
	}
}

func TestLatestExactApprovalReturnsNewestEligibleFullBaseline(t *testing.T) {
	store := New(t.TempDir())
	patch := []byte("diff --git a/app.go b/app.go\n")
	sum := sha256.Sum256(patch)
	target := model.Target{Mode: "branch", BaseSHA: "base", HeadSHA: "head", DiffHash: fmt.Sprintf("%x", sum[:]), Finalizable: true}

	full := writeApprovedBaseline(t, store, time.Unix(1, 0), target, patch, "")
	_ = writeApprovedBaseline(t, store, time.Unix(2, 0), target, patch, "approved-baseline-delta")

	baseline, found, err := store.LatestExactApproval(target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || baseline.Run.ID != full.ID || baseline.Decision.DiffHash != target.DiffHash || string(baseline.Patch) != string(patch) {
		t.Fatalf("approved baseline = %#v, found=%t", baseline, found)
	}
}

func TestLoadApprovedBaselineRejectsNonUnanimousOrUnverifiableRecords(t *testing.T) {
	store := New(t.TempDir())
	patch := []byte("diff --git a/app.go b/app.go\n")
	sum := sha256.Sum256(patch)
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: fmt.Sprintf("%x", sum[:])}
	run := writeApprovedBaseline(t, store, time.Unix(1, 0), target, patch, "")
	manifest, err := LoadManifest(run)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Reviewers[0].Report.Verdict = "request_changes"
	if err := WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedBaseline(run); !errors.Is(err, ErrNotApprovedBaseline) {
		t.Fatalf("non-unanimous baseline error = %v", err)
	}
}

func TestLoadApprovedBaselineIncludesTargetedSecurityApprovals(t *testing.T) {
	store := New(t.TempDir())
	patch := []byte("diff --git a/auth.go b/auth.go\n")
	sum := sha256.Sum256(patch)
	target := model.Target{BaseSHA: "base", HeadSHA: "head", DiffHash: fmt.Sprintf("%x", sum[:])}
	run := writeApprovedBaseline(t, store, time.Unix(1, 0), target, patch, "")
	decision, err := LoadDecision(run)
	if err != nil {
		t.Fatal(err)
	}
	decision.Reviewers = map[string]string{"codex": "approve", "claude": "approve", "claude-security-review": "approve"}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), decision); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(run)
	if err != nil {
		t.Fatal(err)
	}
	securityReport := &model.ReviewReport{Verdict: "approve", ContextComplete: true}
	manifest.SecurityReviews = []model.ReviewerResult{{Reviewer: "claude-security-review", Status: "completed", Report: securityReport}}
	if err := WriteJSON(filepath.Join(run.Path, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedBaseline(run); err != nil {
		t.Fatalf("targeted security approval was not eligible: %v", err)
	}
}

func writeApprovedBaseline(t *testing.T, store Store, started time.Time, target model.Target, patch []byte, scope string) Run {
	t.Helper()
	run, err := store.Create(started, target.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	report := &model.ReviewReport{Verdict: "approve", ContextComplete: true}
	if err := WriteFile(filepath.Join(run.Path, "target.diff"), patch); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(run.Path, "decision.json"), model.Decision{
		RunID: run.ID, State: model.StateApproved, BaseSHA: target.BaseSHA, HeadSHA: target.HeadSHA, DiffHash: target.DiffHash,
		Reviewers: map[string]string{"codex": "approve", "claude": "approve"}, Checks: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(run.Path, "manifest.json"), model.Manifest{
		RunID: run.ID, Target: target, ReviewScope: scope,
		Reviewers: []model.ReviewerResult{
			{Reviewer: "codex", Status: "completed", Report: report},
			{Reviewer: "claude", Status: "completed", Report: report},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestProviderLeaseQueuesAtConcurrencyLimit(t *testing.T) {
	t.Setenv("CORA_PROVIDER_QUEUE_DIR", t.TempDir())
	provider := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	first, err := AcquireProvider(context.Background(), provider, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireProvider(ctx, provider, 1, nil); err == nil {
		t.Fatal("expected second provider lease to queue until its context expired")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireProvider(context.Background(), provider, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
