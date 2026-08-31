package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReviewerDurationUsesMillisecondsInJSON(t *testing.T) {
	contents, err := json.Marshal(ReviewerResult{Reviewer: "codex", Status: "completed", Duration: NewDuration(1500 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if !strings.Contains(text, `"duration_ms":1500`) {
		t.Fatalf("duration JSON = %s", text)
	}
	if strings.Contains(text, `"duration":`) {
		t.Fatalf("duration leaked nanosecond field: %s", text)
	}
}

func TestHeartbeatSeparatesWallAndActiveExecutionMilliseconds(t *testing.T) {
	contents, err := json.Marshal(Heartbeat{
		WallElapsed:       NewDuration(2 * time.Minute),
		ActiveExecution:   NewDuration(45 * time.Second),
		ActiveTimingBasis: "sampled-awake-while-executing",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{`"wall_elapsed_ms":120000`, `"active_execution_ms":45000`, `"active_timing_basis":"sampled-awake-while-executing"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("heartbeat timing JSON does not contain %s: %s", want, text)
		}
	}
}

func TestFindingMarshalEmitsRequiredNullableSchemaProperties(t *testing.T) {
	contents, err := json.Marshal(Finding{
		ID: "minor", Severity: "minor", Confidence: 0.8, File: "app.go", Line: 12,
		Claim: "resource leak", Evidence: "the error return skips Close", SuggestedFix: "defer Close",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{`"disposition":null`, `"reachability":null`} {
		if !strings.Contains(text, want) {
			t.Fatalf("normalized finding omitted required nullable property %s: %s", want, text)
		}
	}
}
