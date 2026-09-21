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
			UpdatedAt: time.Now().UTC(), ActiveTimingBasis: activeTimingBasis, PID: os.Getpid(), Reviewers: map[string]string{}, ReviewerVerdicts: map[string]string{}, ReviewerStartedAt: map[string]time.Time{}, Checks: map[string]string{}, Queues: map[string]model.ProviderQueueStatus{},
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
	h.ReviewerOutcome(name, state, "")
}

func (h *runHeartbeat) ReviewerOutcome(name, state, verdict string) {
	h.mu.Lock()
	h.value.Reviewers[name] = state
	if verdict != "" {
		h.value.ReviewerVerdicts[name] = verdict
	} else {
		// A role may be queued or rerun after an earlier completed attempt.
		// Never expose the prior verdict as if it belonged to the new attempt.
		delete(h.value.ReviewerVerdicts, name)
	}
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
	value.ReviewerVerdicts = cloneStates(h.value.ReviewerVerdicts)
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
	for group, states := range []map[string]string{heartbeat.Reviewers, heartbeat.Checks} {
		names := make([]string, 0, len(states))
		for name := range states {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			detail := name + "=" + states[name]
			if verdict := heartbeat.ReviewerVerdicts[name]; group == 0 && verdict != "" {
				detail += "(verdict=" + verdict + ")"
			}
			if group == 0 && states[name] == "running" && !heartbeat.ReviewerStartedAt[name].IsZero() {
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
		detail := fmt.Sprintf("%s=queue:%d(%s)", name, queue.Position, formatQueueWait(queue, time.Now()))
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
		return "waiting-for-capacity"
	}
	if remaining < time.Second {
		return "<1s"
	}
	return remaining.Round(time.Second).String()
}

func formatQueueWait(status model.ProviderQueueStatus, now time.Time) string {
	parts := make([]string, 0, 1+len(status.Holders))
	if status.ETAAt != nil && status.ETAAt.After(now) {
		parts = append(parts, "eta_in="+formatQueueETA(*status.ETAAt, now))
	}
	for _, holder := range status.Holders {
		identity := holder.Reviewer
		if identity == "" {
			identity = fmt.Sprintf("pid-%d", holder.PID)
		}
		if holder.RunID != "" {
			identity += "@" + holder.RunID
		}
		timeout := "timeout=unknown"
		if holder.TimeoutAt != nil {
			remaining := holder.TimeoutAt.Sub(now)
			if remaining > 0 {
				timeout = "timeout_in=" + formatDuration(remaining)
			} else {
				timeout = "timeout_overdue=" + formatDuration(-remaining)
			}
		}
		parts = append(parts, "holder="+identity+" "+timeout)
	}
	if len(parts) == 0 {
		return "waiting-for-capacity"
	}
	return strings.Join(parts, " ")
}

func wallElapsed(started, ended time.Time) time.Duration {
	duration := ended.UTC().Sub(started.UTC())
	return nonNegativeDuration(duration)
}
