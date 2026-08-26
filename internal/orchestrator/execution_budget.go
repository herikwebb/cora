package orchestrator

import (
	"context"
	"sync"
	"time"
)

// executionBudget counts wall-clock time only while at least one reviewer or
// check is executing. Provider and quota queue time therefore cannot consume
// the run's overall execution timeout.
type executionBudget struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	remaining time.Duration
	active    int
	started   time.Time
	timer     *time.Timer
}

func newExecutionBudget(parent context.Context, duration time.Duration) *executionBudget {
	ctx, cancel := context.WithCancel(parent)
	return &executionBudget{ctx: ctx, cancel: cancel, remaining: duration}
}

func (b *executionBudget) Context() context.Context { return b.ctx }

func (b *executionBudget) Start() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil || b.remaining <= 0 {
		b.cancel()
		return false
	}
	b.active++
	if b.active == 1 {
		b.started = time.Now()
		b.timer = time.AfterFunc(b.remaining, b.cancel)
	}
	return true
}

func (b *executionBudget) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == 0 {
		return
	}
	b.active--
	if b.active != 0 {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.remaining -= time.Since(b.started)
	if b.remaining <= 0 {
		b.cancel()
	}
}

func (b *executionBudget) Close() { b.cancel() }
