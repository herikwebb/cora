package orchestrator

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/record"
)

const heartbeatInterval = 30 * time.Second

type runHeartbeat struct {
	mu            sync.Mutex
	writeMu       sync.Mutex
	run           record.Run
	progress      io.Writer
	value         model.Heartbeat
	activeElapsed func() time.Duration
	stop          chan struct{}
	done          chan struct{}
	once          sync.Once
}

func newRunHeartbeat(run record.Run, started time.Time, progress io.Writer, activeElapsed ...func() time.Duration) *runHeartbeat {
	var elapsed func() time.Duration
	if len(activeElapsed) > 0 {
		elapsed = activeElapsed[0]
	}
	return &runHeartbeat{
		run: run, progress: progress, activeElapsed: elapsed, stop: make(chan struct{}), done: make(chan struct{}),
		value: model.Heartbeat{
			RunID: run.ID, State: "active", Phase: "reviewers", StartedAt: started,
			UpdatedAt: time.Now().UTC(), ActiveTimingBasis: activeTimingBasis, PID: os.Getpid(), Reviewers: map[string]string{}, ReviewerStartedAt: map[string]time.Time{}, Checks: map[string]string{}, Queues: map[string]model.ProviderQueueStatus{},
		},
	}
}

func (h *runHeartbeat) Queue(name string, status model.ProviderQueueStatus) {
	h.mu.Lock()
	h.value.Queues[name] = status
	h.value.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.write()
}

func (h *runHeartbeat) ClearQueue(name string) {
	h.mu.Lock()
	delete(h.value.Queues, name)
	h.value.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.write()
}

func (h *runHeartbeat) Start() {
	h.write()
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				h.write()
				snapshot := h.snapshot()
				if h.progress != nil {
					fmt.Fprintf(h.progress, "cora: run %s wall=%s active-execution=%s (%s)\n", h.run.ID, formatDuration(snapshot.WallElapsed.Duration), formatDuration(snapshot.ActiveExecution.Duration), heartbeatDetail(snapshot))
				}
			case <-h.stop:
				return
			}
		}
	}()
}

func (h *runHeartbeat) Phase(phase string) {
	h.mu.Lock()
	h.value.Phase = phase
	h.value.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.write()
}

func (h *runHeartbeat) Reviewer(name, state string) {
	h.mu.Lock()
	h.value.Reviewers[name] = state
	if state == "running" {
		if h.value.ReviewerStartedAt[name].IsZero() {
			h.value.ReviewerStartedAt[name] = time.Now().UTC()
		}
	} else {
		delete(h.value.ReviewerStartedAt, name)
	}
	h.value.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.write()
}

func (h *runHeartbeat) Check(name, state string) {
	h.mu.Lock()
	h.value.Checks[name] = state
	h.value.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	h.write()
}

func (h *runHeartbeat) Finish(state string) {
	h.once.Do(func() {
		h.mu.Lock()
		h.value.State = state
		h.value.Phase = "finished"
		h.value.UpdatedAt = time.Now().UTC()
		h.mu.Unlock()
		h.write()
		close(h.stop)
		<-h.done
	})
}

func (h *runHeartbeat) write() {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	now := time.Now().UTC()
	h.value.UpdatedAt = now
	h.value.WallElapsed = model.NewDuration(wallElapsed(h.value.StartedAt, now))
	if h.activeElapsed != nil {
		h.value.ActiveExecution = model.NewDuration(h.activeElapsed())
	}
	h.mu.Unlock()
	_ = record.WriteHeartbeat(h.run, h.snapshot())
}

func (h *runHeartbeat) snapshot() model.Heartbeat {
	h.mu.Lock()
	defer h.mu.Unlock()
	value := h.value
	value.Reviewers = cloneStates(h.value.Reviewers)
	value.ReviewerStartedAt = cloneTimes(h.value.ReviewerStartedAt)
	value.Checks = cloneStates(h.value.Checks)
	value.Queues = cloneQueues(h.value.Queues)
	return value
}

func cloneTimes(source map[string]time.Time) map[string]time.Time {
	clone := make(map[string]time.Time, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneQueues(source map[string]model.ProviderQueueStatus) map[string]model.ProviderQueueStatus {
	clone := make(map[string]model.ProviderQueueStatus, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneStates(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func heartbeatDetail(heartbeat model.Heartbeat) string {
	parts := []string{"phase=" + heartbeat.Phase}
	for _, states := range []map[string]string{heartbeat.Reviewers, heartbeat.Checks} {
		names := make([]string, 0, len(states))
		for name := range states {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			detail := name + "=" + states[name]
			if states[name] == "running" && !heartbeat.ReviewerStartedAt[name].IsZero() {
				detail += "(wall=" + formatDuration(wallElapsed(heartbeat.ReviewerStartedAt[name], time.Now())) + ")"
			}
			parts = append(parts, detail)
		}
	}
	queueNames := make([]string, 0, len(heartbeat.Queues))
	for name := range heartbeat.Queues {
		queueNames = append(queueNames, name)
	}
	sort.Strings(queueNames)
	for _, name := range queueNames {
		queue := heartbeat.Queues[name]
		detail := fmt.Sprintf("%s=queue:%d", name, queue.Position)
		if queue.ETAAt != nil {
			detail += "~" + formatQueueETA(*queue.ETAAt, time.Now())
		}
		parts = append(parts, detail)
	}
	return strings.Join(parts, " ")
}

func nonNegativeDuration(duration time.Duration) time.Duration {
	if duration < 0 {
		return 0
	}
	return duration
}

func formatQueueETA(etaAt, now time.Time) string {
	remaining := etaAt.Sub(now)
	if remaining <= 0 {
		return "estimate-exceeded"
	}
	if remaining < time.Second {
		return "<1s"
	}
	return remaining.Round(time.Second).String()
}

func wallElapsed(started, ended time.Time) time.Duration {
	duration := ended.UTC().Sub(started.UTC())
	return nonNegativeDuration(duration)
}
