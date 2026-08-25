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
