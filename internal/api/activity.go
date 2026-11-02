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

func activityWriteInterval(cfg *config.APIConfig) time.Duration {
	if cfg == nil || cfg.ActivityWriteInterval <= 0 {
		return config.DefaultActivityWriteInterval
	}
	return cfg.ActivityWriteInterval
}

// activityLag is how long last_active_at can trail a sandbox's last use: up to
// one interval until the next write starts, and up to one interval for that
// write ("Sandbox activity" in docs/architecture.md).
func activityLag(cfg *config.APIConfig) time.Duration {
	return 2 * activityWriteInterval(cfg)
}

func maxResponseActivity(cfg *config.APIConfig) time.Duration {
	if cfg == nil || cfg.MaxResponseActivity <= 0 {
		return config.DefaultMaxResponseActivity
	}
	return cfg.MaxResponseActivity
}

// ActivityRecorder collects which sandboxes this API pod serves and writes
// their last_active_at in one statement per interval, from Run.
type ActivityRecorder struct {
	store               store.SandboxStore
	interval            time.Duration
	maxResponseActivity time.Duration
	now                 func() time.Time
	writeNow            chan struct{} // asks Run to write before the next tick

	mu          sync.Mutex
	bySandboxID map[string]*sandboxActivity
}

type sandboxActivity struct {
	// numActiveRequests counts this pod's requests to the sandbox whose
	// response is still open.
	numActiveRequests int
	// lastActiveAt is when this pod last saw the sandbox in use, in unix
	// seconds: a response started or ended, or one was still open at a write.
	lastActiveAt int64
	// unwritten is set while lastActiveAt has not been written yet.
	unwritten bool
}

// activeAt records use at t, a unix second. A second already collected for a
// write is not collected again.
func (e *sandboxActivity) activeAt(t int64) {
	if t > e.lastActiveAt {
		e.lastActiveAt = t
		e.unwritten = true
	}
}

func NewActivityRecorder(s store.SandboxStore, cfg *config.APIConfig) *ActivityRecorder {
	return &ActivityRecorder{
		store:               s,
		interval:            activityWriteInterval(cfg),
		maxResponseActivity: maxResponseActivity(cfg),
		now:                 time.Now,
		writeNow:            make(chan struct{}, 1),
		bySandboxID:         make(map[string]*sandboxActivity),
	}
}

// Run writes the collected activity every interval until ctx is done, then
// once more, so a pod that shuts down keeps its last requests.
func (a *ActivityRecorder) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	writeNow := a.writeNow
	for {
		select {
		case <-ctx.Done():
			a.write()
			return
		case <-ticker.C:
		case <-writeNow:
		}
		if a.write() {
			writeNow = a.writeNow
		} else {
			// Retried at the next tick, not once per request.
			writeNow = nil
		}
	}
}

// write stores everything collected since the last write in one statement. A
// failed write is retried by the next one.
func (a *ActivityRecorder) write() (ok bool) {
	batch := a.takeUnwritten()
	if len(batch) == 0 {
		return true
	}
	// One interval at most, so the next write starts on time.
	ctx, cancel := context.WithTimeout(context.Background(), a.interval)
	err := a.store.RecordActivity(ctx, batch)
	cancel()
	if err == nil {
		return true
	}
	slog.Error("record sandbox activity failed", "sandboxes", len(batch), "err", err)
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, at := range batch {
		e := a.entryLocked(id)
		e.lastActiveAt = max(e.lastActiveAt, at)
		e.unwritten = true
	}
	return false
}

// takeUnwritten returns the last_active_at to write for each sandbox, marks
// them written, and forgets sandboxes with nothing new to write.
func (a *ActivityRecorder) takeUnwritten() map[string]int64 {
	now := a.now().Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	batch := make(map[string]int64)
	for id, e := range a.bySandboxID {
		if e.numActiveRequests > 0 {
			e.activeAt(now)
		}
		switch {
		case e.unwritten:
			batch[id] = e.lastActiveAt
			e.unwritten = false
		case e.numActiveRequests == 0:
			delete(a.bySandboxID, id)
		}
	}
	return batch
}

// trackResponse counts a response from the sandbox as activity from now until
// the returned end is called, for at most maxResponseActivity.
func (a *ActivityRecorder) trackResponse(rec *store.SandboxRecord) (end func()) {
	now := a.now().Unix()
	a.mu.Lock()
	e := a.entryLocked(rec.ID)
	e.numActiveRequests++
	e.activeAt(now)
	a.mu.Unlock()

	// A wake, or the first request after a quiet spell (the row is older than
	// the lag, so no pod has written it lately), asks for a write now rather
	// than at the next tick, so GET shows the sandbox running, and the sweeper
	// sees the request, about one database round trip later.
	if rec.Status != "running" || now-rec.LastActiveAt > idleSeconds(2*a.interval) {
		select {
		case a.writeNow <- struct{}{}:
		default: // a write is already due
		}
	}

	// e stays in bySandboxID while numActiveRequests counts this response.
	var once sync.Once
	finish := func(ended bool) {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			e.numActiveRequests--
			if ended {
				e.activeAt(a.now().Unix())
			}
		})
	}
	// So a client that stops reading cannot keep its sandbox from idling.
	timer := time.AfterFunc(a.maxResponseActivity, func() { finish(false) })
	return func() {
		timer.Stop()
		finish(true)
	}
}

func (a *ActivityRecorder) entryLocked(id string) *sandboxActivity {
	e, ok := a.bySandboxID[id]
	if !ok {
		e = &sandboxActivity{}
		a.bySandboxID[id] = e
	}
	return e
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
