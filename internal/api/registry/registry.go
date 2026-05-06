package registry

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrNoRunners is returned when no runners are registered or eligible for placement.
var ErrNoRunners = errors.New("no sandbox runners are registered or available")

// Runner describes a registered sandbox runner.
type Runner struct {
	ID              string
	HTTPBaseURL     string // Endpoints proxied to the sandbox
	ControlGRPCAddr string // Sandbox lifecycle methods
	Healthy         bool
	CapacityTotal   int32
	CapacityUsed    int32
	LastSeen        time.Time
}

// Registry tracks runners that connect via gRPC and supports round-robin placement.
type Registry struct {
	mu             sync.Mutex         // protects runners, order, and rrCounter
	runners        map[string]*Runner // keyed by runner ID
	order          []string           // first-registration order of runner IDs
	rrCounter      uint64             // round-robin cursor into eligible list
	heartbeatGrace time.Duration      // max age of LastSeen for placement eligibility
}

// New returns an empty registry. heartbeatGrace must be positive (how long after LastSeen a runner stays eligible).
func New(heartbeatGrace time.Duration) *Registry {
	if heartbeatGrace <= 0 {
		heartbeatGrace = 45 * time.Second
	}
	return &Registry{
		runners:        make(map[string]*Runner),
		heartbeatGrace: heartbeatGrace,
	}
}

// Upsert updates or inserts a runner from a heartbeat. The caller must reject invalid
// httpBaseURL before calling (e.g. gRPC registration validates with IsValidRunnerHTTPBaseURL).
func (r *Registry) Upsert(id, httpBaseURL, controlGRPCAddr string, healthy bool, capTotal, capUsed int32) {
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
		LastSeen:        time.Now(),
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

// eligibleLocked returns healthy runners with spare capacity and a recent heartbeat; caller holds r.mu.
func (r *Registry) eligibleLocked(now time.Time) []*Runner {
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
