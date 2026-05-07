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

// ExecSession represents a running or completed exec command whose lifetime
// is independent of any particular HTTP connection.
type ExecSession struct {
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

func (sess *ExecSession) getNextSeq() uint64 {
	seq := sess.nextSeq
	sess.nextSeq++
	return seq
}

func (sess *ExecSession) append(resp Response) {
	sess.mu.Lock()

	seq := sess.getNextSeq()
	resp.Seq = &seq

	data, _ := json.Marshal(resp)
	sess.events = append(sess.events, storedEvent{seq: seq, data: data})
	sess.size += int64(len(data))

	for sess.size > sess.maxEventBytes && len(sess.events) > 1 {
		sess.size -= int64(len(sess.events[0].data))
		sess.events[0] = storedEvent{} // nil out to allow GC of the data byte slice
		sess.events = sess.events[1:]
		sess.minSeq = sess.events[0].seq
	}

	if resp.isTerminal() {
		sess.completed = true
		sess.completedAt = time.Now()
	}

	old := sess.notify
	sess.notify = make(chan struct{})
	sess.mu.Unlock()
	close(old)
}

// eventsAfter returns pre-marshaled data for retained events with seq > after.
// If after is nil, all retained events are returned. Caller must hold sess.mu.
func (sess *ExecSession) eventsAfter(after *uint64) [][]byte {
	var result [][]byte
	for _, e := range sess.events {
		if after == nil || e.seq > *after {
			result = append(result, e.data)
		}
	}
	return result
}

// hasHistoryAfter reports whether all events after the given seq are retained.
// If after is nil (requesting all events), checks that the earliest event is
// still available. Caller must hold sess.mu.
func (sess *ExecSession) hasHistoryAfter(after *uint64) bool {
	if after == nil {
		return sess.minSeq == 0
	}
	return *after+1 >= sess.minSeq
}

// HasHistory reports whether events after the given seq are still retained.
// If after is nil, checks whether all events from the start are available.
func (sess *ExecSession) HasHistory(after *uint64) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.hasHistoryAfter(after)
}

// Follow streams pre-marshaled JSON events to callback for events with seq > after
// (or all events if after is nil). Blocks until a terminal event is sent or ctx
// is cancelled. Returns false if requested history has been trimmed (410).
func (sess *ExecSession) Follow(ctx context.Context, after *uint64, callback func([]byte)) bool {
	for {
		sess.mu.Lock()

		if !sess.hasHistoryAfter(after) {
			sess.mu.Unlock()
			return false
		}

		batch := sess.eventsAfter(after)
		completed := sess.completed
		notify := sess.notify

		if len(batch) > 0 {
			lastSeq := sess.events[len(sess.events)-1].seq
			after = &lastSeq
		}

		sess.mu.Unlock()

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
func (sess *ExecSession) Snapshot(after *uint64) ([][]byte, bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if !sess.hasHistoryAfter(after) {
		return nil, false
	}

	return sess.eventsAfter(after), true
}
