package registry

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// MemoryRegistry tracks runners in-process (single API pod / SQLite mode).
type MemoryRegistry struct {
	mu             sync.Mutex
	runners        map[string]*Runner
	order          []string
	rrCounter      uint64
	heartbeatGrace time.Duration
	removedAt      map[string]time.Time
}

// NewMemory returns an empty in-memory registry.
func NewMemory(heartbeatGrace time.Duration) *MemoryRegistry {
	if heartbeatGrace <= 0 {
		heartbeatGrace = 45 * time.Second
	}
	return &MemoryRegistry{
		runners:        make(map[string]*Runner),
		heartbeatGrace: heartbeatGrace,
		removedAt:      make(map[string]time.Time),
	}
}

// New returns an empty in-memory registry. Alias for NewMemory.
func New(heartbeatGrace time.Duration) *MemoryRegistry {
	return NewMemory(heartbeatGrace)
}

func (r *MemoryRegistry) Upsert(id, httpBaseURL, controlGRPCAddr string, healthy bool, capTotal, capUsed, capStopped int32) {
	r.UpsertRunner(Runner{ID: id, HTTPBaseURL: httpBaseURL, ControlGRPCAddr: controlGRPCAddr, Healthy: healthy,
		CapacityTotal: capTotal, CapacityUsed: capUsed, CapacityStopped: capStopped})
}

func (r *MemoryRegistry) UpsertRunner(run Runner) {
	if run.ID == "" || run.HTTPBaseURL == "" {
		return
	}
	run.HTTPBaseURL = strings.TrimRight(run.HTTPBaseURL, "/")
	run.ControlGRPCAddr = strings.TrimSpace(run.ControlGRPCAddr)
	run.Templates = slices.Clone(run.Templates)
	run.LastSeen = time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.runners[run.ID]; !exists {
		r.order = append(r.order, run.ID)
	}
	r.runners[run.ID] = &run
	delete(r.removedAt, run.ID)
}

func (r *MemoryRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" {
		return
	}
	delete(r.runners, id)
	r.removedAt[id] = time.Now()
	out := r.order[:0]
	for _, x := range r.order {
		if x != id {
			out = append(out, x)
		}
	}
	r.order = out
}

func (r *MemoryRegistry) Get(id string) (*Runner, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runners[id]
	if !ok {
		return nil, false
	}
	return run, true
}

func (r *MemoryRegistry) GoneLongEnough(runnerID string, buffer time.Duration, now time.Time) bool {
	if runnerID == "" || buffer <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runners[runnerID]; ok {
		return false
	}
	goneAt, ok := r.removedAt[runnerID]
	if !ok {
		return false
	}
	return !now.Before(goneAt.Add(buffer))
}

func (r *MemoryRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runners)
}

func (r *MemoryRegistry) List() ([]Runner, error) {
	r.mu.Lock()
	out := make([]Runner, 0, len(r.runners))
	for _, run := range r.runners {
		out = append(out, *run)
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Runner) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (r *MemoryRegistry) eligibleLocked(now time.Time) []*Runner {
	var eligible []*Runner
	for _, id := range r.order {
		run, ok := r.runners[id]
		if !ok {
			continue
		}
		if now.Sub(run.LastSeen) > r.heartbeatGrace {
			continue
		}
		if !run.Healthy || run.Cordoned {
			continue
		}
		if run.CapacityTotal > 0 && run.CapacityUsed >= run.CapacityTotal {
			continue
		}
		eligible = append(eligible, run)
	}
	return eligible
}

func (r *MemoryRegistry) PickLowestUsed(exclude ...string) (*Runner, error) {
	return r.PickForTemplate(DefaultTemplate, exclude...)
}

func (r *MemoryRegistry) PickForTemplate(template string, exclude ...string) (*Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var best *Runner
	for _, run := range r.eligibleLocked(time.Now()) {
		if slices.Contains(exclude, run.ID) || !run.Serves(template) {
			continue
		}
		if best == nil || run.CapacityUsed < best.CapacityUsed ||
			(run.CapacityUsed == best.CapacityUsed && run.ID < best.ID) {
			best = run
		}
	}
	if best == nil {
		return nil, ErrNoRunners
	}
	return best, nil
}

// PickRoundRobin returns the next eligible runner in registration order.
func (r *MemoryRegistry) PickRoundRobin() (*Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	eligible := r.eligibleLocked(now)
	if len(eligible) == 0 {
		return nil, ErrNoRunners
	}

	idx := int(r.rrCounter % uint64(len(eligible)))
	r.rrCounter++
	return eligible[idx], nil
}
