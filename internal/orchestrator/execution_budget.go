package orchestrator

import (
	"context"
	"sync"
	"time"
)

const (
	executionSampleInterval = time.Second
	activeTimingBasis       = "sampled-awake-while-executing"
	// A ticker gap this large cannot be distinguished portably from machine
	// suspend. Credit only one sample for it so sleep does not exhaust the
	// execution budget immediately after the machine wakes.
	executionSuspendGap = 5 * time.Second
)

// executionBudget counts sampled awake time only while at least one reviewer
// or check is executing. Provider and quota queue time therefore cannot consume
// the run's overall execution timeout. Long sampling gaps are treated as likely
// machine suspend; this is deliberately best-effort because Go does not expose
// a portable suspend notification.
type executionBudget struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	remaining time.Duration
	elapsed   time.Duration
	active    int
	observed  time.Time
	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newExecutionBudget(parent context.Context, duration time.Duration) *executionBudget {
	ctx, cancel := context.WithCancel(parent)
	budget := &executionBudget{
		ctx: ctx, cancel: cancel, remaining: duration,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go budget.sample()
	return budget
}

func (b *executionBudget) Context() context.Context { return b.ctx }

func (b *executionBudget) Start() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil || b.remaining <= 0 {
		b.cancel()
		return false
	}
	now := time.Now()
	b.observeLocked(now)
	b.active++
	if b.active == 1 {
		b.observed = now
	}
	b.notify()
	return true
}

func (b *executionBudget) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == 0 {
		return
	}
	b.observeLocked(time.Now())
	b.active--
	if b.active == 0 {
		b.observed = time.Time{}
	}
	b.notify()
}

// Elapsed returns the sampled awake duration for which one or more execution
// tasks were active. Concurrent tasks count once toward the run-wide budget.
func (b *executionBudget) Elapsed() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observeLocked(time.Now())
	return b.elapsed
}

func (b *executionBudget) Close() {
	b.closeOnce.Do(func() {
		b.cancel()
		<-b.done
	})
}

func (b *executionBudget) sample() {
	defer close(b.done)
	for {
		b.mu.Lock()
		active := b.active > 0
		remaining := b.remaining
		b.mu.Unlock()

		var timer *time.Timer
		var timerC <-chan time.Time
		if active && remaining > 0 {
			interval := min(executionSampleInterval, remaining)
			timer = time.NewTimer(interval)
			timerC = timer.C
		}
		select {
		case now := <-timerC:
			b.mu.Lock()
			b.observeLocked(now)
			b.mu.Unlock()
		case <-b.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-b.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

func (b *executionBudget) observeLocked(now time.Time) {
	if b.active == 0 || b.observed.IsZero() {
		return
	}
	delta := now.Sub(b.observed)
	if delta < 0 {
		delta = 0
	}
	credited := delta
	if delta > executionSuspendGap {
		credited = min(executionSampleInterval, delta)
	}
	credited = min(credited, b.remaining)
	b.elapsed += credited
	b.remaining -= credited
	b.observed = now
	if b.remaining <= 0 {
		b.cancel()
	}
}

func (b *executionBudget) notify() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}
