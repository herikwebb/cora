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
	heartbeat.Reviewer("codex", "completed")
	completed, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.ReviewerStartedAt["codex"].IsZero() {
		t.Fatalf("completed reviewer retained running timestamp: %#v", completed.ReviewerStartedAt)
	}
}

func TestHeartbeatQueueETAReportsExceededEstimateInsteadOfZero(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	detail := heartbeatDetail(model.Heartbeat{
		Phase:  "reviewers",
		Queues: map[string]model.ProviderQueueStatus{"claude": {Position: 1, ETAAt: &deadline}},
	})
	if !strings.Contains(detail, "estimate-exceeded") || strings.Contains(detail, "~0s") {
		t.Fatalf("expired queue ETA detail = %q", detail)
	}
}
