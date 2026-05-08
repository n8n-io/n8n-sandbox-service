package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type storedEvent struct {
	seq  uint64
	data []byte // pre-marshaled JSON line (no trailing newline)
}

// Execution represents a running or completed command execution whose lifetime
// is independent of any particular HTTP connection.
type Execution struct {
	ID string

	mu            sync.Mutex
	events        []storedEvent
	size          int64
	maxEventBytes int64
	// minSeq is the lowest seq still retained; events before it were trimmed.
	minSeq  uint64
	nextSeq uint64

	completed   bool
	completedAt time.Time
	cancel      context.CancelFunc
	// notify implements a broadcast signal for Follow waiters. When append
	// stores a new event it closes the current channel (which unblocks every
	// goroutine selecting on it) and replaces it with a fresh one for the
	// next wait cycle. A closed channel cannot be reused, so a new one is
	// required after each broadcast. Follow grabs a reference to the current
	// channel under the lock, then selects on it after releasing the lock.
	notify chan struct{}
}

func (ex *Execution) getNextSeq() uint64 {
	seq := ex.nextSeq
	ex.nextSeq++
	return seq
}

func (ex *Execution) append(resp Response) {
	ex.mu.Lock()

	seq := ex.getNextSeq()
	resp.Seq = &seq

	data, _ := json.Marshal(resp)
	ex.events = append(ex.events, storedEvent{seq: seq, data: data})
	ex.size += int64(len(data))

	for ex.size > ex.maxEventBytes && len(ex.events) > 1 {
		ex.size -= int64(len(ex.events[0].data))
		ex.events[0] = storedEvent{} // nil out to allow GC of the data byte slice
		ex.events = ex.events[1:]
		ex.minSeq = ex.events[0].seq
	}

	if resp.isTerminal() {
		ex.completed = true
		ex.completedAt = time.Now()
	}

	old := ex.notify
	ex.notify = make(chan struct{})
	ex.mu.Unlock()
	close(old)
}

// eventsAfter returns pre-marshaled data for retained events with seq > after.
// If after is nil, all retained events are returned. Caller must hold ex.mu.
func (ex *Execution) eventsAfter(after *uint64) [][]byte {
	start := 0
	if after != nil {
		start = int(*after + 1 - ex.minSeq)
	}
	if start >= len(ex.events) {
		return nil
	}
	tail := ex.events[start:]
	result := make([][]byte, len(tail))
	for i, e := range tail {
		result[i] = e.data
	}
	return result
}

// hasHistoryAfter reports whether all events after the given seq are retained.
// If after is nil (requesting all events), checks that the earliest event is
// still available. Caller must hold ex.mu.
func (ex *Execution) hasHistoryAfter(after *uint64) bool {
	if after == nil {
		return ex.minSeq == 0
	}
	return *after+1 >= ex.minSeq
}

// HasHistory reports whether events after the given seq are still retained.
// If after is nil, checks whether all events from the start are available.
func (ex *Execution) HasHistory(after *uint64) bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.hasHistoryAfter(after)
}

// Follow streams pre-marshaled JSON events to callback for events with seq > after
// (or all events if after is nil). Blocks until a terminal event is sent or ctx
// is cancelled. Returns false if requested history has been trimmed (410).
func (ex *Execution) Follow(ctx context.Context, after *uint64, callback func([]byte)) bool {
	for {
		ex.mu.Lock()

		if !ex.hasHistoryAfter(after) {
			ex.mu.Unlock()
			return false
		}

		batch := ex.eventsAfter(after)
		completed := ex.completed
		notify := ex.notify

		if len(batch) > 0 {
			lastSeq := ex.events[len(ex.events)-1].seq
			after = &lastSeq
		}

		ex.mu.Unlock()

		for _, data := range batch {
			callback(data)
		}

		if completed {
			return true
		}

		select {
		case <-ctx.Done():
			return true
		case <-notify:
		}
	}
}

// Snapshot returns pre-marshaled JSON events with seq > after (or all if after
// is nil) without blocking. Returns false if requested history has been trimmed.
func (ex *Execution) Snapshot(after *uint64) ([][]byte, bool) {
	ex.mu.Lock()
	defer ex.mu.Unlock()

	if !ex.hasHistoryAfter(after) {
		return nil, false
	}

	return ex.eventsAfter(after), true
}
