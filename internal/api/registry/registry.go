// Package registry tracks registered runners (in memory or in Postgres) and
// picks a runner for new sandboxes.
package registry

import (
	"errors"
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
	CapacityStopped int32
	// Cordoned: an operator drained the runner, so placement skips it.
	Cordoned bool
	// Templates the runner serves; empty means "default" alone.
	Templates []Template
	LastSeen  time.Time
}

// Template is a template a runner serves, with what one sandbox of it gets.
type Template struct {
	Name     string `json:"name"`
	VCPUs    int32  `json:"vcpus"`
	MemoryMB int64  `json:"memory_mb"`
	DiskMB   int64  `json:"disk_mb"`
}

// DefaultTemplate is the template of a create that names none.
const DefaultTemplate = "default"

// Serves reports whether the runner can boot template ("" is the default).
func (r *Runner) Serves(template string) bool {
	if template == "" {
		template = DefaultTemplate
	}
	if len(r.Templates) == 0 {
		return template == DefaultTemplate
	}
	for _, t := range r.Templates {
		if t.Name == template {
			return true
		}
	}
	return false
}

// RunnerRegistry tracks runners and supports load-aware placement.
type RunnerRegistry interface {
	Upsert(id, httpBaseURL, controlGRPCAddr string, healthy bool, capTotal, capUsed, capStopped int32)
	// UpsertRunner records a heartbeat; LastSeen is set to now.
	UpsertRunner(run Runner)
	Remove(id string)
	Get(id string) (*Runner, bool)
	GoneLongEnough(runnerID string, buffer time.Duration, now time.Time) bool
	Len() int
	// List returns every registered runner, stale ones included, ordered by ID.
	List() ([]Runner, error)
	// PickLowestUsed returns the eligible runner with the lowest capacity_used,
	// skipping the runners in exclude. It places the default template.
	PickLowestUsed(exclude ...string) (*Runner, error)
	// PickForTemplate is PickLowestUsed among the runners serving template.
	PickForTemplate(template string, exclude ...string) (*Runner, error)
}

var _ RunnerRegistry = (*PostgresRegistry)(nil)
var _ RunnerRegistry = (*MemoryRegistry)(nil)
