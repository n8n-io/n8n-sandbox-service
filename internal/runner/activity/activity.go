// Package activity tracks which sandboxes on this runner are doing work: an
// execution stream is open, or an execution the runner started has not been
// seen to end. The API's idle sweeper treats them as active, so a long run is
// never stopped under its caller, and drain reports a runner drained only once
// none is left. The state is in memory: a restarted runner starts empty.
package activity

import (
	"sync"
	"time"
)

// Grace is added to an execution's timeout before the tracker gives up on
// seeing it end, covering the daemon's kill and the exit event.
const Grace = time.Minute

// Tracker is safe for concurrent use. The zero value is not usable; call New.
// A nil Tracker tracks nothing, for callers that do not need it.
type Tracker struct {
	mu      sync.Mutex
	streams map[string]int
	execs   map[string]map[string]*execState
	now     func() time.Time
}

type execState struct {
	// deadline: past it the process has been killed by its own timeout.
	deadline time.Time
	// seq is the last event seen, so a poll reads only what came after it.
	seq *uint64
}

// Pending is an execution whose end nobody is watching.
type Pending struct {
	ExecID string
	// Seq is the last event seen, nil for none.
	Seq *uint64
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{
		streams: map[string]int{},
		execs:   map[string]map[string]*execState{},
		now:     time.Now,
	}
}

// StreamOpened counts an open execution stream until the returned func is
// called; calling it more than once is harmless.
func (t *Tracker) StreamOpened(sandboxID string) (closed func()) {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	t.streams[sandboxID]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.streams[sandboxID]--; t.streams[sandboxID] <= 0 {
				delete(t.streams, sandboxID)
			}
		})
	}
}

// ExecStarted records an execution that runs for at most timeout.
func (t *Tracker) ExecStarted(sandboxID, execID string, timeout time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.execs[sandboxID]
	if m == nil {
		m = map[string]*execState{}
		t.execs[sandboxID] = m
	}
	deadline := t.now().Add(timeout + Grace)
	if st := m[execID]; st != nil {
		st.deadline = deadline
		return
	}
	m[execID] = &execState{deadline: deadline}
}

// ExecProgress records the last event seen of a running execution.
func (t *Tracker) ExecProgress(sandboxID, execID string, seq uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.execs[sandboxID][execID]; st != nil {
		st.seq = &seq
	}
}

// ExecEnded forgets an execution that was seen to end.
func (t *Tracker) ExecEnded(sandboxID, execID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := t.execs[sandboxID]; m != nil {
		delete(m, execID)
		if len(m) == 0 {
			delete(t.execs, sandboxID)
		}
	}
}

// Forget drops a sandbox's executions: it was stopped or deleted, which ends
// them. Open streams are left to close on their own.
func (t *Tracker) Forget(sandboxID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.execs, sandboxID)
}

// Snapshot is the runner's activity at one moment.
type Snapshot struct {
	// Active lists every sandbox with an open stream or an unfinished
	// execution, in no particular order.
	Active []string
	// Executions counts unfinished executions; Streams counts open streams.
	Executions, Streams int
}

// Snapshot drops executions past their deadline and reports the rest.
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	var s Snapshot
	seen := map[string]bool{}
	for id, n := range t.streams {
		s.Streams += n
		seen[id] = true
		s.Active = append(s.Active, id)
	}
	for id, m := range t.execs {
		s.Executions += len(m)
		if !seen[id] {
			s.Active = append(s.Active, id)
		}
	}
	return s
}

// Active reports whether a sandbox has an open stream or an unfinished
// execution.
func (t *Tracker) Active(sandboxID string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	return t.streams[sandboxID] > 0 || len(t.execs[sandboxID]) > 0
}

// Unwatched returns, per sandbox with no open stream, the executions whose end
// nobody is watching, so the caller can ask the daemon whether they ended.
func (t *Tracker) Unwatched() map[string][]Pending {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	out := map[string][]Pending{}
	for id, m := range t.execs {
		if t.streams[id] > 0 {
			continue
		}
		for execID, st := range m {
			p := Pending{ExecID: execID}
			if st.seq != nil {
				seq := *st.seq
				p.Seq = &seq
			}
			out[id] = append(out[id], p)
		}
	}
	return out
}

func (t *Tracker) expireLocked() {
	now := t.now()
	for id, m := range t.execs {
		for execID, st := range m {
			if now.After(st.deadline) {
				delete(m, execID)
			}
		}
		if len(m) == 0 {
			delete(t.execs, id)
		}
	}
}
