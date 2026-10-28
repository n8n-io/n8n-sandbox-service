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
	// Cordoned is set by an operator on the runner: it keeps its sandboxes but
	// takes no new ones.
	Cordoned        bool
	CapacityTotal   int32
	CapacityUsed    int32
	CapacityStopped int32
	LastSeen        time.Time
}

// RunnerRegistry tracks runners and supports load-aware placement.
type RunnerRegistry interface {
	Upsert(id, httpBaseURL, controlGRPCAddr string, healthy, cordoned bool, capTotal, capUsed, capStopped int32)
	Remove(id string)
	Get(id string) (*Runner, bool)
	GoneLongEnough(runnerID string, buffer time.Duration, now time.Time) bool
	Len() int
	// List returns every registered runner, stale ones included, ordered by ID.
	List() ([]Runner, error)
	// PickLowestUsed returns the eligible runner with the lowest capacity_used,
	// skipping the runners in exclude. Unhealthy, cordoned, full and stale
	// runners are not eligible.
	PickLowestUsed(exclude ...string) (*Runner, error)
}
