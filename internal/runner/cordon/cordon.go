// Package cordon holds whether an operator has cordoned the runner: it keeps
// serving its sandboxes but takes no new creates. The state lives in memory
// only, so a restarted runner is always uncordoned.
package cordon

import "sync"

// State is safe for concurrent use. The zero value is not usable; call New.
type State struct {
	mu       sync.Mutex
	cordoned bool
	changed  chan struct{}
}

// New returns an uncordoned State.
func New() *State {
	return &State{changed: make(chan struct{}, 1)}
}

// Cordoned reports whether the runner is cordoned.
func (s *State) Cordoned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cordoned
}

// Set cordons or uncordons the runner and reports whether that changed the state.
func (s *State) Set(cordoned bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cordoned == cordoned {
		return false
	}
	s.cordoned = cordoned
	// Changes that arrive while one is pending collapse into it: the reader only
	// needs to know it should look again.
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return true
}

// Changed receives after the state changes, so the heartbeat can carry it
// without waiting for the next tick.
func (s *State) Changed() <-chan struct{} {
	return s.changed
}
