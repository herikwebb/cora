package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/record"
)

func TestHeartbeatTracksRunningReviewerElapsedTimeAndRefreshesTimestamp(t *testing.T) {
	run := record.Run{ID: "run", Path: t.TempDir()}
	heartbeat := newRunHeartbeat(run, time.Now().Add(-time.Minute), nil, func() time.Duration { return 12 * time.Second })
	heartbeat.Reviewer("codex", "running")
	first, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReviewerStartedAt["codex"].IsZero() || !strings.Contains(heartbeatDetail(first), "codex=running(wall=") {
		t.Fatalf("running reviewer heartbeat = %#v", first)
	}
	if first.WallElapsed.Duration < time.Minute || first.ActiveExecution.Duration != 12*time.Second || first.ActiveTimingBasis != activeTimingBasis {
		t.Fatalf("heartbeat timing = wall %s active %s basis %q", first.WallElapsed.Duration, first.ActiveExecution.Duration, first.ActiveTimingBasis)
	}
	time.Sleep(2 * time.Millisecond)
	heartbeat.write()
	second, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("heartbeat timestamp did not refresh: first=%s second=%s", first.UpdatedAt, second.UpdatedAt)
	}
	heartbeat.ReviewerOutcome("codex", "completed", "approve")
	completed, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.ReviewerStartedAt["codex"].IsZero() {
		t.Fatalf("completed reviewer retained running timestamp: %#v", completed.ReviewerStartedAt)
	}
	if completed.ReviewerVerdicts["codex"] != "approve" || !strings.Contains(heartbeatDetail(completed), "codex=completed(verdict=approve)") {
		t.Fatalf("completed reviewer verdict was not exposed: %#v", completed)
	}
	heartbeat.Reviewer("codex", "queued")
	queued, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if queued.ReviewerVerdicts["codex"] != "" || strings.Contains(heartbeatDetail(queued), "verdict=") {
		t.Fatalf("queued retry retained stale verdict: %#v", queued)
	}
	heartbeat.Reviewer("codex", "running")
	runningAgain, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if runningAgain.ReviewerVerdicts["codex"] != "" || runningAgain.ReviewerStartedAt["codex"].IsZero() {
		t.Fatalf("running retry retained stale verdict or lost start time: %#v", runningAgain)
	}
}

func TestHeartbeatQueueShowsCapacityHolderAfterETA(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	timeoutAt := time.Now().Add(2 * time.Minute)
	detail := heartbeatDetail(model.Heartbeat{
		Phase: "reviewers",
		Queues: map[string]model.ProviderQueueStatus{"claude": {
			Position: 1, ETAAt: &deadline,
			Holders: []model.ProviderCapacityHolder{{RunID: "run-active", Reviewer: "claude", TimeoutAt: &timeoutAt}},
		}},
	})
	for _, want := range []string{"holder=claude@run-active", "timeout_in="} {
		if !strings.Contains(detail, want) {
			t.Fatalf("capacity-holder detail %q does not contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "estimate-exceeded") {
		t.Fatalf("expired historical estimate remained in detail: %q", detail)
	}
}
