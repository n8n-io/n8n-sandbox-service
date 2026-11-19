package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Record is one metered model request, as the usageBatch webhook defines it.
type Record struct {
	RecordID                 string            `json:"record_id"`
	SandboxID                string            `json:"sandbox_id"`
	TenantID                 string            `json:"tenant_id"`
	Labels                   map[string]string `json:"labels"`
	Provider                 Provider          `json:"provider"`
	Host                     string            `json:"host"`
	Path                     string            `json:"path"`
	Model                    string            `json:"model,omitempty"`
	ProviderRequestID        string            `json:"provider_request_id,omitempty"`
	Status                   int               `json:"status"`
	StartedAt                time.Time         `json:"started_at"`
	EndedAt                  time.Time         `json:"ended_at"`
	Streamed                 bool              `json:"streamed"`
	Complete                 bool              `json:"complete"`
	InputTokens              int64             `json:"input_tokens"`
	OutputTokens             int64             `json:"output_tokens"`
	CacheReadInputTokens     int64             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64             `json:"cache_creation_input_tokens"`
}

// NewRecordID returns a dedupe key for a record.
func NewRecordID() string { return uuid.NewString() }

type batch struct {
	BatchID  string   `json:"batch_id"`
	RunnerID string   `json:"runner_id"`
	Records  []Record `json:"records"`
}

const (
	maxBatchRecords = 500
	flushInterval   = 10 * time.Second
	retryFor        = time.Hour
	// maxQueued bounds what waits in memory for the sink; past it the oldest
	// records are dropped and logged. maxQueuedPerSandbox keeps one sandbox
	// from pushing out the others: past it, that sandbox's new records are.
	maxQueued           = 100_000
	maxQueuedPerSandbox = 10_000
)

// Sink delivers records to the usage sink at least once: a batch is retried,
// with the same batch_id, until the sink acknowledges it or an hour passes.
type Sink struct {
	url, token, runnerID string
	client               *http.Client

	mu      sync.Mutex
	queue   []Record
	perSbx  map[string]int
	wake    chan struct{}
	dropped int
}

// NewSink posts to url with the bearer token.
func NewSink(url, token, runnerID string) *Sink {
	return &Sink{
		url: url, token: token, runnerID: runnerID,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// The sink is configuration, not a guest choice; still, a redirect
			// would carry the token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		wake:   make(chan struct{}, 1),
		perSbx: map[string]int{},
	}
}

// Add queues a record.
func (s *Sink) Add(r Record) {
	s.mu.Lock()
	if s.perSbx[r.SandboxID] >= maxQueuedPerSandbox {
		s.dropped++
		s.mu.Unlock()
		return
	}
	if len(s.queue) >= maxQueued {
		s.forget(s.queue[0])
		s.queue = s.queue[1:]
		s.dropped++
	}
	s.queue = append(s.queue, r)
	s.perSbx[r.SandboxID]++
	full := len(s.queue) >= maxBatchRecords
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Run delivers batches until ctx ends.
func (s *Sink) Run(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		for s.deliverOne(ctx) {
		}
	}
}

// deliverOne sends the oldest batch; it reports whether more are waiting.
func (s *Sink) deliverOne(ctx context.Context) bool {
	s.mu.Lock()
	if s.dropped > 0 {
		slog.Error("usage records dropped: the sink is behind", "dropped", s.dropped)
		s.dropped = 0
	}
	n := min(len(s.queue), maxBatchRecords)
	if n == 0 {
		s.mu.Unlock()
		return false
	}
	b := batch{BatchID: uuid.NewString(), RunnerID: s.runnerID, Records: append([]Record(nil), s.queue[:n]...)}
	for _, r := range b.Records {
		s.forget(r)
	}
	s.queue = s.queue[n:]
	more := len(s.queue) > 0
	s.mu.Unlock()

	if !s.post(ctx, b) {
		// Given up on, or ctx ended: put the records back only in the latter
		// case, so a shutdown does not count as delivered.
		if ctx.Err() != nil {
			s.mu.Lock()
			s.queue = append(b.Records, s.queue...)
			for _, r := range b.Records {
				s.perSbx[r.SandboxID]++
			}
			s.mu.Unlock()
		}
		return false
	}
	return more
}

// post sends one batch until it is acknowledged, refused for good, or retryFor
// has passed. It reports whether the sink took it.
func (s *Sink) post(ctx context.Context, b batch) bool {
	body, err := json.Marshal(b)
	if err != nil {
		slog.Error("usage batch encode failed", "err", err)
		return true
	}
	deadline := time.Now().Add(retryFor)
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		wait, err := s.postOnce(ctx, body)
		if err == nil {
			return true
		}
		if wait < 0 {
			slog.Error("usage batch refused by the sink, dropped", "batch_id", b.BatchID, "records", len(b.Records), "err", err)
			return true
		}
		if time.Now().After(deadline) {
			slog.Error("usage batch undeliverable for an hour, dropped", "batch_id", b.BatchID, "records", len(b.Records), "err", err)
			return true
		}
		if wait == 0 {
			wait = backoff
			backoff = min(backoff*2, time.Minute)
		}
		slog.Warn("usage batch delivery failed, retrying", "batch_id", b.BatchID, "attempt", attempt, "retry_in", wait, "err", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// postOnce returns nil on a 2xx. Otherwise it returns how long to wait before
// retrying (0: backoff), or -1 when retrying cannot help.
func (s *Sink) postOnce(ctx context.Context, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return 0, nil
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		wait := time.Duration(0)
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait = min(time.Duration(secs)*time.Second, 10*time.Minute)
		}
		return wait, fmt.Errorf("sink answered %d", resp.StatusCode)
	default:
		return -1, fmt.Errorf("sink answered %d", resp.StatusCode)
	}
}

func (s *Sink) forget(r Record) {
	if s.perSbx[r.SandboxID]--; s.perSbx[r.SandboxID] <= 0 {
		delete(s.perSbx, r.SandboxID)
	}
}

// Flush delivers the oldest waiting batch now; it reports whether more wait.
func (s *Sink) Flush(ctx context.Context) bool { return s.deliverOne(ctx) }
