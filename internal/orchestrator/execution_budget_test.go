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

func TestExecutionBudgetDoesNotChargeLikelyMachineSuspendGap(t *testing.T) {
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	budget := &executionBudget{
		remaining: time.Hour,
		active:    1,
		observed:  started,
	}
	budget.observeLocked(started.Add(20 * time.Minute))
	if budget.elapsed != executionSampleInterval {
		t.Fatalf("likely suspend gap charged %s, want one %s sample", budget.elapsed, executionSampleInterval)
	}
	if budget.remaining != time.Hour-executionSampleInterval {
		t.Fatalf("remaining budget = %s", budget.remaining)
	}
}

func TestExecutionBudgetChargesOrdinaryActiveIntervalsExactly(t *testing.T) {
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	budget := &executionBudget{
		remaining: time.Hour,
		active:    1,
		observed:  started,
	}
	budget.observeLocked(started.Add(3 * time.Second))
	if budget.elapsed != 3*time.Second {
		t.Fatalf("ordinary active interval charged %s", budget.elapsed)
	}
}
