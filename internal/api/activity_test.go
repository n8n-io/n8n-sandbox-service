package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

type activityWrite struct {
	id string
	at int64
}

// activityStore records RecordActivity calls and fails them while fail is set.
// The recorder needs no other store method.
type activityStore struct {
	store.SandboxStore
	mu    sync.Mutex
	calls []activityWrite
	fail  error
}

func (s *activityStore) RecordActivity(_ context.Context, id string, at int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, activityWrite{id: id, at: at})
	return s.fail == nil, s.fail
}

func (s *activityStore) setFail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// drain returns the calls since the last drain, sorted by sandbox id.
func (s *activityStore) drain() []activityWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls
	s.calls = nil
	slices.SortFunc(calls, func(a, b activityWrite) int { return strings.Compare(a.id, b.id) })
	return calls
}

// t0 is the test recorder's starting clock.
const t0 = int64(1_000_000)

func newTestActivityRecorder(t *testing.T, cfg *config.APIConfig) (*ActivityRecorder, *activityStore, func(int64)) {
	t.Helper()
	s := &activityStore{}
	a := NewActivityRecorder(s, cfg)
	var mu sync.Mutex
	clock := t0
	a.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Unix(clock, 0)
	}
	setClock := func(sec int64) {
		mu.Lock()
		defer mu.Unlock()
		clock = sec
	}
	return a, s, setClock
}

func wantWrites(t *testing.T, a *ActivityRecorder, s *activityStore, want ...activityWrite) {
	t.Helper()
	a.writes.Wait()
	got := s.drain()
	if len(got) != len(want) || (len(want) > 0 && !slices.Equal(got, want)) {
		t.Fatalf("writes = %+v, want %+v", got, want)
	}
}

func row(id, status string, lastActiveAt int64) *store.SandboxRecord {
	return &store.SandboxRecord{ID: id, Status: status, LastActiveAt: lastActiveAt}
}

// A request writes last_active_at unless it was written in the last interval,
// as the row shows or as this pod remembers, or the sandbox was not running.
func TestActivityRecorderWritesUnlessRecentlyWritten(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil) // 30s interval

	a.begin(row("fresh", "running", t0-29))()
	wantWrites(t, a, s)

	a.begin(row("stale", "running", t0-30))()
	wantWrites(t, a, s, activityWrite{id: "stale", at: t0})

	// A burst read the old row before the first write landed.
	for range 100 {
		a.begin(row("stale", "running", t0-30))()
	}
	wantWrites(t, a, s)

	a.begin(row("stopped", "stopped", t0))()
	wantWrites(t, a, s, activityWrite{id: "stopped", at: t0})

	setClock(t0 + 30)
	a.begin(row("stale", "running", t0-30))()
	wantWrites(t, a, s, activityWrite{id: "stale", at: t0 + 30})
}

func TestActivityRecorderRetriesAfterAFailedWrite(t *testing.T) {
	a, s, _ := newTestActivityRecorder(t, nil)
	s.setFail(errors.New("database unavailable"))
	a.begin(row("sb", "running", 1))()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0})

	s.setFail(nil)
	a.begin(row("sb", "running", 1))()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0})
}

// Requests that skipped their write because one was in flight relied on it, so
// when it fails the next tick writes the latest request, response open or not.
func TestActivityRecorderTickRetriesAFailedWriteWithTheLatestRequest(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil)
	s.setFail(errors.New("database unavailable"))
	a.begin(row("sb", "running", 1))()
	setClock(t0 + 2)
	a.begin(row("sb", "running", 1))()
	a.writes.Wait()
	s.drain()

	s.setFail(nil)
	setClock(t0 + 290)
	a.writeOpen()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0 + 2})
	a.writeOpen()
	wantWrites(t, a, s)

	// The retry wrote the old request time, so a request now is new activity.
	setClock(t0 + 300)
	a.begin(row("sb", "running", t0+2))()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0 + 300})
}

func TestActivityRecorderKeepsSubSecondIntervals(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, &config.APIConfig{ActivityWriteInterval: 1500 * time.Millisecond})
	a.begin(row("sb", "running", 1))()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0})
	setClock(t0 + 1)
	a.begin(row("sb", "running", 1))()
	wantWrites(t, a, s)
	setClock(t0 + 2)
	a.begin(row("sb", "running", 1))()
	wantWrites(t, a, s, activityWrite{id: "sb", at: t0 + 2})
}

// blockingActivityStore holds every RecordActivity until release is closed.
type blockingActivityStore struct {
	store.SandboxStore
	calls, inFlight, peak atomic.Int32
	release               chan struct{}
	mu                    sync.Mutex
	written               []activityWrite
}

func (s *blockingActivityStore) RecordActivity(_ context.Context, id string, at int64) (bool, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.written = append(s.written, activityWrite{id: id, at: at})
	s.mu.Unlock()
	n := s.inFlight.Add(1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	<-s.release
	s.inFlight.Add(-1)
	return true, nil
}

// A tick with many open responses must not take every database connection.
func TestActivityRecorderBoundsConcurrentWrites(t *testing.T) {
	s := &blockingActivityStore{release: make(chan struct{})}
	a := NewActivityRecorder(s, nil)
	for i := range 3 * activityWriters {
		a.begin(row(fmt.Sprintf("sb-%d", i), "stopped", 0))()
	}
	waitFor(t, "the writers to fill", func() bool { return s.inFlight.Load() == activityWriters })
	time.Sleep(20 * time.Millisecond)
	close(s.release)
	a.writes.Wait()
	if p := s.peak.Load(); p != activityWriters {
		t.Fatalf("peak concurrent writes = %d, want %d", p, activityWriters)
	}
}

// A response still streaming is written every interval, so a long command
// keeps its sandbox from going idle. Once it ends, it stops counting.
func TestActivityRecorderWritesOpenResponsesEveryInterval(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil)
	end := a.begin(row("sb", "running", t0))
	wantWrites(t, a, s)

	for _, at := range []int64{t0 + 30, t0 + 60} {
		setClock(at)
		a.writeOpen()
		wantWrites(t, a, s, activityWrite{id: "sb", at: at})
	}

	end()
	end() // the proxy may close a body twice
	setClock(t0 + 90)
	a.writeOpen()
	wantWrites(t, a, s)
	if _, ok := a.sandboxes["sb"]; ok {
		t.Fatal("sandbox still tracked an interval after its last write")
	}
}

func TestActivityRecorderStopsCountingAResponseAfterMaxResponseActivity(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, &config.APIConfig{MaxResponseActivity: 20 * time.Millisecond})
	end := a.begin(row("sb", "running", t0))
	defer end()
	waitFor(t, "the response to stop counting", func() bool { return a.inflight("sb") == 0 })

	setClock(t0 + 30)
	a.writeOpen()
	wantWrites(t, a, s)
}

// A write that waited for a writer sends the latest request, so the requests
// that arrived meanwhile, and skipped their own write, are not lost.
func TestActivityRecorderQueuedWriteSendsTheLatestRequest(t *testing.T) {
	s := &blockingActivityStore{release: make(chan struct{})}
	a := NewActivityRecorder(s, nil)
	var clock atomic.Int64
	clock.Store(t0)
	a.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	for i := range activityWriters {
		a.begin(row(fmt.Sprintf("busy-%d", i), "stopped", 0))()
	}
	waitFor(t, "the writers to fill", func() bool { return s.inFlight.Load() == activityWriters })
	a.begin(row("queued", "stopped", 0))()
	clock.Store(t0 + 10)
	a.begin(row("queued", "stopped", 0))()

	close(s.release)
	a.writes.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	var queued []activityWrite
	for _, w := range s.written {
		if w.id == "queued" {
			queued = append(queued, w)
		}
	}
	if len(queued) != 1 || queued[0].at != t0+10 {
		t.Fatalf("queued writes = %+v, want one with the latest request %d", queued, t0+10)
	}
}

// On stop, Run waits for the writes the database is running and drops the
// ones still queued for a writer.
func TestActivityRecorderRunWaitsForRunningWritesAndDropsQueuedOnes(t *testing.T) {
	s := &blockingActivityStore{release: make(chan struct{})}
	a := NewActivityRecorder(s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	for i := range activityWriters + 1 {
		a.begin(row(fmt.Sprintf("sb-%d", i), "stopped", 0))()
	}
	waitFor(t, "the writers to fill", func() bool { return s.inFlight.Load() == activityWriters })

	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while writes were running")
	case <-time.After(50 * time.Millisecond):
	}
	close(s.release)
	<-done
	if n := s.calls.Load(); n != activityWriters {
		t.Fatalf("writes = %d, want %d: the queued one should be dropped", n, activityWriters)
	}
	a.begin(row("sb-late", "stopped", 0))()
	if n := s.calls.Load(); n != activityWriters {
		t.Fatalf("writes after stop = %d, want none new", n)
	}
}

// A write that gets a writer slot after the stop does not run, even when the
// slot frees up before it sees done.
func TestActivityRecorderSkipsAWriteThatGetsASlotAfterStop(t *testing.T) {
	s := &blockingActivityStore{release: make(chan struct{})}
	a := NewActivityRecorder(s, nil)
	for i := range activityWriters + 1 {
		a.begin(row(fmt.Sprintf("sb-%d", i), "stopped", 0))()
	}
	waitFor(t, "the writers to fill", func() bool { return s.inFlight.Load() == activityWriters })
	a.mu.Lock()
	a.stopped = true // without closing done, so the freed slot wins
	a.mu.Unlock()
	close(s.release)
	a.writes.Wait()
	if n := s.calls.Load(); n != activityWriters {
		t.Fatalf("writes = %d, want %d: the one queued past the stop must not run", n, activityWriters)
	}
}

func TestActivityLagIsTwiceTheWriteIntervalPlusTheWriteTimeout(t *testing.T) {
	if got := activityLag(nil); got != 65*time.Second {
		t.Fatalf("default lag = %s, want 65s", got)
	}
	if got := activityLag(&config.APIConfig{ActivityWriteInterval: time.Second}); got != 7*time.Second {
		t.Fatalf("lag = %s, want 7s", got)
	}
}
