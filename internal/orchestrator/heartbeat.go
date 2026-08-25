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
	mu       sync.Mutex
	writeMu  sync.Mutex
	run      record.Run
	progress io.Writer
	value    model.Heartbeat
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func newRunHeartbeat(run record.Run, started time.Time, progress io.Writer) *runHeartbeat {
	return &runHeartbeat{
		run: run, progress: progress, stop: make(chan struct{}), done: make(chan struct{}),
		value: model.Heartbeat{
			RunID: run.ID, State: "active", Phase: "reviewers", StartedAt: started,
			UpdatedAt: time.Now().UTC(), PID: os.Getpid(), Reviewers: map[string]string{}, Checks: map[string]string{},
		},
	}
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
					fmt.Fprintf(h.progress, "cora: run %s active for %s (%s)\n", h.run.ID, formatDuration(time.Since(snapshot.StartedAt)), heartbeatDetail(snapshot))
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
	_ = record.WriteHeartbeat(h.run, h.snapshot())
}

func (h *runHeartbeat) snapshot() model.Heartbeat {
	h.mu.Lock()
	defer h.mu.Unlock()
	value := h.value
	value.Reviewers = cloneStates(h.value.Reviewers)
	value.Checks = cloneStates(h.value.Checks)
	return value
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
			parts = append(parts, name+"="+states[name])
		}
	}
	return strings.Join(parts, " ")
}
