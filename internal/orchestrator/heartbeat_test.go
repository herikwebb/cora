package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/herikwebb/cora/internal/record"
)

func TestHeartbeatTracksRunningReviewerElapsedTimeAndRefreshesTimestamp(t *testing.T) {
	run := record.Run{ID: "run", Path: t.TempDir()}
	heartbeat := newRunHeartbeat(run, time.Now().Add(-time.Minute), nil)
	heartbeat.Reviewer("codex", "running")
	first, err := record.LoadHeartbeat(run)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReviewerStartedAt["codex"].IsZero() || !strings.Contains(heartbeatDetail(first), "codex=running(") {
		t.Fatalf("running reviewer heartbeat = %#v", first)
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
