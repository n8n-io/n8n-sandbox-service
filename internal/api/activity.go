package api

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

const (
	activityWriteTimeout = 5 * time.Second
	activityWriters      = 4
)

func activityWriteInterval(cfg *config.APIConfig) time.Duration {
	if cfg == nil || cfg.ActivityWriteInterval <= 0 {
		return config.DefaultActivityWriteInterval
	}
	return cfg.ActivityWriteInterval
}

// activityLag: see "Sandbox activity" in docs/architecture.md.
func activityLag(cfg *config.APIConfig) time.Duration {
	return 2*activityWriteInterval(cfg) + activityWriteTimeout
}

func maxResponseActivity(cfg *config.APIConfig) time.Duration {
	if cfg == nil || cfg.MaxResponseActivity <= 0 {
		return config.DefaultMaxResponseActivity
	}
	return cfg.MaxResponseActivity
}

// ActivityRecorder writes last_active_at in the background. How and when:
// "Sandbox activity" in docs/architecture.md.
type ActivityRecorder struct {
	store    store.SandboxStore
	interval time.Duration
	maxOpen  time.Duration
	now      func() time.Time
	writes   sync.WaitGroup
	writers  chan struct{}
	done     chan struct{} // closed on stop: queued writes give up

	mu        sync.Mutex
	sandboxes map[string]*sandboxActivity
	stopped   bool
}

type sandboxActivity struct {
	open    int       // responses still streaming
	last    int64     // the latest request on this pod, unix seconds
	written time.Time // when this pod last wrote last_active_at
	writing bool
	retry   bool // a write failed; the next tick writes last
}

func NewActivityRecorder(s store.SandboxStore, cfg *config.APIConfig) *ActivityRecorder {
	return &ActivityRecorder{
		store:     s,
		interval:  activityWriteInterval(cfg),
		maxOpen:   maxResponseActivity(cfg),
		now:       time.Now,
		writers:   make(chan struct{}, activityWriters),
		done:      make(chan struct{}),
		sandboxes: make(map[string]*sandboxActivity),
	}
}

func (a *ActivityRecorder) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.stopped = true
			close(a.done)
			a.mu.Unlock()
			a.writes.Wait()
			return
		case <-ticker.C:
			a.writeOpen()
		}
	}
}

func (a *ActivityRecorder) writeOpen() {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.sandboxes {
		switch {
		case e.open > 0:
			a.writeLocked(id, e, now.Unix())
		case e.retry:
			a.writeLocked(id, e, e.last)
		case !e.writing && now.Sub(e.written) >= a.interval:
			delete(a.sandboxes, id)
		}
	}
}

func (a *ActivityRecorder) begin(rec *store.SandboxRecord) (end func()) {
	now := a.now()
	a.mu.Lock()
	e := a.entry(rec.ID)
	e.open++
	e.last = max(e.last, now.Unix())
	recent := now.Sub(time.Unix(rec.LastActiveAt, 0)) < a.interval || now.Sub(e.written) < a.interval
	if rec.Status != "running" || !recent {
		a.writeLocked(rec.ID, e, now.Unix())
	}
	a.mu.Unlock()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			a.mu.Lock()
			a.entry(rec.ID).open--
			a.mu.Unlock()
		})
	}
	// So a client that stops reading cannot keep its sandbox from idling.
	timer := time.AfterFunc(a.maxOpen, stop)
	return func() {
		timer.Stop()
		stop()
	}
}

func (a *ActivityRecorder) entry(id string) *sandboxActivity {
	e, ok := a.sandboxes[id]
	if !ok {
		e = &sandboxActivity{}
		a.sandboxes[id] = e
	}
	return e
}

// writeLocked writes at, or a later request's time, as last_active_at in the
// background. e.written is always a time the row will hold once it succeeds.
func (a *ActivityRecorder) writeLocked(id string, e *sandboxActivity, at int64) {
	if e.writing || a.stopped {
		return
	}
	prev := e.written
	e.writing, e.written, e.retry = true, time.Unix(at, 0), false
	a.writes.Add(1)
	go func() {
		defer a.writes.Done()
		var err error
		select {
		case a.writers <- struct{}{}:
			a.mu.Lock()
			if a.stopped { // select may pick the slot over done
				a.mu.Unlock()
				<-a.writers
				return
			}
			// Include the requests that arrived while this write waited.
			e := a.entry(id)
			at = max(at, e.last)
			if e.open > 0 {
				at = max(at, a.now().Unix())
			}
			e.written = time.Unix(at, 0)
			a.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), activityWriteTimeout)
			_, err = a.store.RecordActivity(ctx, id, at)
			cancel()
			<-a.writers
		case <-a.done:
			return
		}
		a.mu.Lock()
		e := a.entry(id)
		e.writing = false
		if err != nil {
			// Requests skipped while this write was in flight relied on it.
			e.written, e.retry = prev, true
		}
		a.mu.Unlock()
		if err != nil {
			slog.Error("record sandbox activity failed", "sandbox_id", id, "err", err)
		}
	}()
}

// The reverse proxy always closes the body, so end always runs.
type activityBody struct {
	io.ReadCloser
	end func()
}

func (b activityBody) Close() error {
	err := b.ReadCloser.Close()
	b.end()
	return err
}
