package registry

import (
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNoRunners is returned when no runners are registered or eligible for placement.
var ErrNoRunners = errors.New("no sandbox runners are registered or available")

const heartbeatGrace = 45 * time.Second

// Runner describes a registered sandbox runner.
type Runner struct {
	ID            string
	HTTPBaseURL   string
	Healthy       bool
	CapacityTotal int32
	CapacityUsed  int32
	LastSeen      time.Time
}

// Registry tracks runners that connect via gRPC and supports round-robin placement.
type Registry struct {
	mu        sync.Mutex
	runners   map[string]*Runner // keyed by runner ID
	order     []string           // first-registration order of runner IDs
	rrCounter uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		runners: make(map[string]*Runner),
	}
}

// Upsert updates or inserts a runner from a heartbeat.
func (r *Registry) Upsert(id, httpBaseURL string, healthy bool, capTotal, capUsed int32) {
	if id == "" || httpBaseURL == "" {
		return
	}
	httpBaseURL = strings.TrimRight(httpBaseURL, "/")
	if _, err := url.Parse(httpBaseURL); err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.runners[id]; !exists {
		r.order = append(r.order, id)
	}

	r.runners[id] = &Runner{
		ID:            id,
		HTTPBaseURL:   httpBaseURL,
		Healthy:       healthy,
		CapacityTotal: capTotal,
		CapacityUsed:  capUsed,
		LastSeen:      time.Now(),
	}
}

// Remove drops a runner (e.g. stream closed).
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runners, id)
	out := r.order[:0]
	for _, x := range r.order {
		if x != id {
			out = append(out, x)
		}
	}
	r.order = out
}

func (r *Registry) eligibleLocked(now time.Time) []*Runner {
	var eligible []*Runner
	for _, id := range r.order {
		run, ok := r.runners[id]
		if !ok {
			continue
		}
		if now.Sub(run.LastSeen) > heartbeatGrace {
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

// PickRoundRobin returns the next eligible runner (registration order preserved in eligible list).
func (r *Registry) PickRoundRobin() (*Runner, error) {
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
