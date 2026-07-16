package registry

import (
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
	if id == "" || httpBaseURL == "" {
		return
	}
	httpBaseURL = strings.TrimRight(httpBaseURL, "/")
	controlGRPCAddr = strings.TrimSpace(controlGRPCAddr)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.runners[id]; !exists {
		r.order = append(r.order, id)
	}

	r.runners[id] = &Runner{
		ID:              id,
		HTTPBaseURL:     httpBaseURL,
		ControlGRPCAddr: controlGRPCAddr,
		Healthy:         healthy,
		CapacityTotal:   capTotal,
		CapacityUsed:    capUsed,
		CapacityStopped: capStopped,
		LastSeen:        time.Now(),
	}
	delete(r.removedAt, id)
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
		if !run.Healthy {
			continue
		}
		if run.CapacityTotal > 0 && run.CapacityUsed >= run.CapacityTotal {
			continue
		}
		eligible = append(eligible, run)
	}
	return eligible
}

func (r *MemoryRegistry) PickLowestUsed() (*Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	eligible := r.eligibleLocked(now)
	if len(eligible) == 0 {
		return nil, ErrNoRunners
	}

	best := eligible[0]
	for _, run := range eligible[1:] {
		if run.CapacityUsed < best.CapacityUsed ||
			(run.CapacityUsed == best.CapacityUsed && run.ID < best.ID) {
			best = run
		}
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
