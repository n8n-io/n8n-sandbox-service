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
	tick     time.Duration
	interval int64 // seconds
	maxOpen  time.Duration
	now      func() time.Time
	writes   sync.WaitGroup
	writers  chan struct{}

	mu        sync.Mutex
	sandboxes map[string]*sandboxActivity
	stopped   bool
}

type sandboxActivity struct {
	open    int   // responses still streaming
	last    int64 // the latest request on this pod
	written int64 // when this pod last wrote last_active_at
	writing bool
	retry   bool // a write failed; the next tick writes last
}

func NewActivityRecorder(s store.SandboxStore, cfg *config.APIConfig) *ActivityRecorder {
	interval := activityWriteInterval(cfg)
	return &ActivityRecorder{
		store:     s,
		tick:      interval,
		interval:  int64(interval / time.Second),
		maxOpen:   maxResponseActivity(cfg),
		now:       time.Now,
		writers:   make(chan struct{}, activityWriters),
		sandboxes: make(map[string]*sandboxActivity),
	}
}

func (a *ActivityRecorder) Run(ctx context.Context) {
	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.stopped = true
			a.mu.Unlock()
			a.writes.Wait()
			return
		case <-ticker.C:
			a.writeOpen()
		}
	}
}

func (a *ActivityRecorder) writeOpen() {
	now := a.now().Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.sandboxes {
		switch {
		case e.open > 0:
			a.writeLocked(id, e, now)
		case e.retry:
			a.writeLocked(id, e, e.last)
		case !e.writing && now-e.written >= a.interval:
			delete(a.sandboxes, id)
		}
	}
}

func (a *ActivityRecorder) begin(rec *store.SandboxRecord) (end func()) {
	now := a.now().Unix()
	a.mu.Lock()
	e := a.entry(rec.ID)
	e.open++
	e.last = max(e.last, now)
	recent := now-rec.LastActiveAt < a.interval || now-e.written < a.interval
	if rec.Status != "running" || !recent {
		a.writeLocked(rec.ID, e, now)
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

func (a *ActivityRecorder) writeLocked(id string, e *sandboxActivity, at int64) {
	if e.writing || a.stopped {
		return
	}
	prev := e.written
	e.writing, e.written, e.retry = true, at, false
	a.writes.Add(1)
	go func() {
		defer a.writes.Done()
		ctx, cancel := context.WithTimeout(context.Background(), activityWriteTimeout)
		var err error
		select {
		case a.writers <- struct{}{}:
			_, err = a.store.RecordActivity(ctx, id, at)
			<-a.writers
		case <-ctx.Done():
			err = ctx.Err()
		}
		cancel()
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
