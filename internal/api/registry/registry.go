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
	LastSeen        time.Time
}

// RunnerRegistry tracks runners and supports load-aware placement.
type RunnerRegistry interface {
	Upsert(id, httpBaseURL, controlGRPCAddr string, healthy bool, capTotal, capUsed, capStopped int32)
	Remove(id string)
	Get(id string) (*Runner, bool)
	GoneLongEnough(runnerID string, buffer time.Duration, now time.Time) bool
	Len() int
	PickLowestUsed() (*Runner, error)
}
