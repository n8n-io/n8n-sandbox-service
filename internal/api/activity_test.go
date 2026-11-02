package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// activityStore records the batches RecordActivity gets, and fails them while
// fail is set. The recorder needs no other store method.
type activityStore struct {
	store.SandboxStore
	mu      sync.Mutex
	batches []map[string]int64
	timeout time.Duration // left on the context of the last call
	fail    error
}

func (s *activityStore) RecordActivity(ctx context.Context, batch map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, maps.Clone(batch))
	if deadline, ok := ctx.Deadline(); ok {
		s.timeout = time.Until(deadline)
	}
	return s.fail
}

func (s *activityStore) setFail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

func (s *activityStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

// drain returns the batches since the last drain.
func (s *activityStore) drain() []map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches := s.batches
	s.batches = nil
	return batches
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

func wantBatches(t *testing.T, s *activityStore, want ...map[string]int64) {
	t.Helper()
	got := s.drain()
	if len(got) != len(want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	for i := range want {
		if !maps.Equal(got[i], want[i]) {
			t.Fatalf("batches = %v, want %v", got, want)
		}
	}
}

func row(id, status string, lastActiveAt int64) *store.SandboxRecord {
	return &store.SandboxRecord{ID: id, Status: status, LastActiveAt: lastActiveAt}
}

func (a *ActivityRecorder) tracked(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.bySandboxID[id]
	return ok
}

// A burst of requests costs one row in one statement, with the latest request.
func TestActivityRecorderWritesEachSandboxOnceWithItsLatestRequest(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil)
	a.write()
	wantBatches(t, s) // nothing to write, no statement

	for range 100 {
		a.trackResponse(row("a", "running", t0))()
	}
	setClock(t0 + 2)
	a.trackResponse(row("a", "running", t0))()
	a.trackResponse(row("b", "running", t0))()
	a.write()
	wantBatches(t, s, map[string]int64{"a": t0 + 2, "b": t0 + 2})

	// A second already written is not written again.
	a.trackResponse(row("a", "running", t0))()
	a.write()
	wantBatches(t, s)
	if a.tracked("a") || a.tracked("b") {
		t.Fatal("sandboxes still tracked after a write with nothing new")
	}
}

func TestActivityRecorderRetriesAFailedWriteWithTheLatestRequest(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil)
	s.setFail(errors.New("database unavailable"))
	a.trackResponse(row("a", "running", t0))()
	a.trackResponse(row("b", "running", t0))()
	a.write()
	wantBatches(t, s, map[string]int64{"a": t0, "b": t0})

	s.setFail(nil)
	setClock(t0 + 3)
	a.trackResponse(row("a", "running", t0))()
	a.write()
	wantBatches(t, s, map[string]int64{"a": t0 + 3, "b": t0})
	a.write()
	wantBatches(t, s)
}

// A response still streaming is written at every write, so a long command
// keeps its sandbox from going idle. When it ends, the end is written once.
func TestActivityRecorderWritesOpenResponsesUntilTheyEnd(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, nil)
	end := a.trackResponse(row("sb", "running", t0))
	for _, at := range []int64{t0, t0 + 5, t0 + 10} {
		setClock(at)
		a.write()
		wantBatches(t, s, map[string]int64{"sb": at})
	}

	setClock(t0 + 12)
	end()
	end() // the proxy may close a body twice
	setClock(t0 + 15)
	a.write()
	wantBatches(t, s, map[string]int64{"sb": t0 + 12})
	a.write()
	wantBatches(t, s)
	if a.tracked("sb") {
		t.Fatal("sandbox still tracked after its response ended and was written")
	}
}

// Past MaxResponseActivity a response stops counting, so a client that stops
// reading cannot keep its sandbox from idling. Its end then counts for nothing.
func TestActivityRecorderStopsCountingAResponseAfterMaxResponseActivity(t *testing.T) {
	a, s, setClock := newTestActivityRecorder(t, &config.APIConfig{MaxResponseActivity: 20 * time.Millisecond})
	end := a.trackResponse(row("sb", "running", t0))
	a.write()
	wantBatches(t, s, map[string]int64{"sb": t0})
	waitFor(t, "the response to stop counting", func() bool { return a.numActiveRequests("sb") == 0 })

	setClock(t0 + 5)
	a.write()
	wantBatches(t, s)
	end()
	a.write()
	wantBatches(t, s)
}

// A wake, or a request after a quiet spell, asks Run to write at once.
func TestActivityRecorderAsksForAWriteOnWakeAndAfterQuiet(t *testing.T) {
	a, _, _ := newTestActivityRecorder(t, nil) // 5s interval: a 10s lag
	asked := func() bool {
		select {
		case <-a.writeNow:
			return true
		default:
			return false
		}
	}
	for _, tc := range []struct {
		name string
		rec  *store.SandboxRecord
		want bool
	}{
		{"running, written within the lag", row("fresh", "running", t0-10), false},
		{"running, written before the lag", row("quiet", "running", t0-11), true},
		{"stopped", row("woken", "stopped", t0), true},
	} {
		a.trackResponse(tc.rec)()
		if got := asked(); got != tc.want {
			t.Fatalf("%s: asked for a write = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Run writes every interval, when asked, and once more when it stops; each
// write may take up to one interval.
func TestActivityRecorderRunWritesOnTickOnRequestAndOnStop(t *testing.T) {
	run := func(t *testing.T, interval time.Duration) (*ActivityRecorder, *activityStore, func()) {
		t.Helper()
		a, s, _ := newTestActivityRecorder(t, &config.APIConfig{ActivityWriteInterval: interval})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			a.Run(ctx)
			close(done)
		}()
		stop := func() {
			cancel()
			<-done
		}
		t.Cleanup(stop)
		return a, s, stop
	}

	t.Run("tick", func(t *testing.T) {
		a, s, _ := run(t, 10*time.Millisecond)
		a.trackResponse(row("sb", "running", t0))()
		waitFor(t, "a tick to write", func() bool { return s.count() == 1 })
		wantBatches(t, s, map[string]int64{"sb": t0})
	})

	t.Run("asked and stop", func(t *testing.T) {
		a, s, stop := run(t, time.Hour)
		a.trackResponse(row("woken", "stopped", t0))()
		waitFor(t, "the wake to be written", func() bool { return s.count() == 1 })
		a.trackResponse(row("later", "running", t0))()
		stop()
		wantBatches(t, s, map[string]int64{"woken": t0}, map[string]int64{"later": t0})
		if s.timeout <= 59*time.Minute || s.timeout > time.Hour {
			t.Fatalf("write timeout = %s, want one interval (1h)", s.timeout)
		}
	})
}

// After a failed write, Run waits for the next tick instead of writing again
// for every wake, so a database that refuses writes is not asked once per
// request.
func TestActivityRecorderRunRetriesAFailedWriteAtTheNextTickOnly(t *testing.T) {
	a, s, _ := newTestActivityRecorder(t, &config.APIConfig{ActivityWriteInterval: time.Hour})
	s.setFail(errors.New("database is read-only"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	a.trackResponse(row("first", "stopped", t0))()
	waitFor(t, "the failed write", func() bool { return s.count() == 1 })
	for i := range 20 {
		a.trackResponse(row(fmt.Sprintf("woken-%d", i), "stopped", t0))()
	}
	time.Sleep(50 * time.Millisecond)
	if n := s.count(); n != 1 {
		t.Fatalf("writes after a failed one = %d, want 1 until the next tick", n)
	}
}

// Requests may arrive while a write runs; none of them is lost.
func TestActivityRecorderKeepsRequestsThatArriveDuringWrites(t *testing.T) {
	a, s, _ := newTestActivityRecorder(t, nil)
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
				a.write()
			}
		}
	}()
	var requests sync.WaitGroup
	for i := range 8 {
		requests.Go(func() {
			for range 200 {
				a.trackResponse(row(fmt.Sprintf("sb-%d", i%3), "running", t0))()
			}
		})
	}
	requests.Wait()
	close(stop)
	<-writerDone
	a.write()

	written := make(map[string]int64)
	for _, b := range s.drain() {
		maps.Copy(written, b)
	}
	if want := map[string]int64{"sb-0": t0, "sb-1": t0, "sb-2": t0}; !maps.Equal(written, want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
}

func TestActivityLagIsTwiceTheWriteInterval(t *testing.T) {
	if got := activityLag(nil); got != 10*time.Second {
		t.Fatalf("default lag = %s, want 10s", got)
	}
	if got := activityLag(&config.APIConfig{ActivityWriteInterval: time.Second}); got != 2*time.Second {
		t.Fatalf("lag = %s, want 2s", got)
	}
}
