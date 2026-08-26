package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestExecutionBudgetPausesWhileWorkIsQueued(t *testing.T) {
	budget := newExecutionBudget(context.Background(), 120*time.Millisecond)
	defer budget.Close()
	if !budget.Start() {
		t.Fatal("budget did not start")
	}
	time.Sleep(20 * time.Millisecond)
	budget.Stop()
	time.Sleep(140 * time.Millisecond)
	if err := budget.Context().Err(); err != nil {
		t.Fatalf("paused queue time consumed execution budget: %v", err)
	}
	if !budget.Start() {
		t.Fatal("budget did not resume")
	}
	select {
	case <-budget.Context().Done():
	case <-time.After(150 * time.Millisecond):
		t.Fatal("resumed execution budget did not expire")
	}
}
