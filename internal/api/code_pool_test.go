package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const testPoolKey = "n8n-js/s2/m256/spare1/reuse1"

func poolTestConfig(size int) *config.APIConfig {
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.DefaultMaxSandboxes = 50
	cfg.CodePoolRefillConcurrency = 2
	cfg.CodePoolMaxAge = time.Hour
	cfg.CodePools = []config.CodePoolConfig{{Runtime: "n8n-js", Slots: 2, TaskMemoryMB: 256, ReuseWorkers: true, Size: size}}
	return cfg
}

func newPoolTestRegistry(t *testing.T, fake *fakeSandboxControl, runners ...string) registry.RunnerRegistry {
	t.Helper()
	reg := registry.New(45 * time.Second)
	addr := startFakeRunnerControl(t, fake)
	for _, id := range runners {
		reg.Upsert(id, "https://127.0.0.1:9", addr, true, 100, 0, 0)
	}
	return reg
}

func countByRunner(recs []*store.SandboxRecord) map[string]int {
	m := make(map[string]int)
	for _, r := range recs {
		m[r.RunnerID]++
	}
	return m
}

func TestCodePoolKey(t *testing.T) {
	opts, err := codeCreateOptions("", 2, 256, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := codePoolKey(opts); got != testPoolKey {
		t.Fatalf("key = %q, want %q", got, testPoolKey)
	}
	if _, err := newCodePool(newSweepStore(t), registry.New(time.Minute), &config.APIConfig{
		CodePools: []config.CodePoolConfig{{Slots: 99, Size: 1}},
	}, nil); err == nil {
		t.Fatal("invalid pool options: want an error")
	}
}

// The manager fills the pool and spreads it over the runners. A create request
// with the same options claims a pooled sandbox; other options create one.
func TestCodePoolRefillAndClaim(t *testing.T) {
	fake := &fakeSandboxControl{}
	s := newSweepStore(t)
	reg := newPoolTestRegistry(t, fake, "runner-a", "runner-b")
	cfg := poolTestConfig(4)
	p, err := newCodePool(s, reg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	activeCodePool.Store(p)
	t.Cleanup(func() { activeCodePool.Store(nil) })
	ctx := context.Background()

	p.reconcile(ctx, time.Now(), true)
	p.wg.Wait()
	pooled, err := s.ListPooled()
	if err != nil || len(pooled) != 4 {
		t.Fatalf("pooled = %d (%v), want 4", len(pooled), err)
	}
	if by := countByRunner(pooled); by["runner-a"] != 2 || by["runner-b"] != 2 {
		t.Fatalf("placement = %v, want 2 on each runner", by)
	}
	for _, r := range pooled {
		if r.TenantID != store.PoolTenantID || r.PoolKey != testPoolKey || r.Kind != store.KindCode {
			t.Fatalf("pooled record %+v", r)
		}
	}
	// A second pass does not over-fill.
	p.reconcile(ctx, time.Now(), true)
	p.wg.Wait()
	if pooled, _ := s.ListPooled(); len(pooled) != 4 {
		t.Fatalf("pooled after second pass = %d, want 4", len(pooled))
	}

	router, err := NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	createsBefore := len(fake.createOptionsJSON)
	fake.mu.Unlock()

	rr := postCode(t, router, `{"runtime":"n8n-js","slots":2,"taskMemoryMb":256,"reuseWorkers":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	var resp codeSandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Pooled || resp.Timings == nil || resp.State != "ready" || resp.Slots != 2 {
		t.Fatalf("claim response %+v", resp)
	}
	if resp.ID != pooled[0].ID {
		t.Fatalf("claimed %s, want the oldest %s", resp.ID, pooled[0].ID)
	}
	rec, _ := s.Get(resp.ID)
	if rec == nil || rec.TenantID != store.AdminTenantID || rec.PoolKey != "" {
		t.Fatalf("claimed record %+v", rec)
	}

	rr = postCode(t, router, `{"runtime":"n8n-js","slots":1,"taskMemoryMb":256,"reuseWorkers":true}`)
	if rr.Code != http.StatusCreated || strings.Contains(rr.Body.String(), `"pooled"`) {
		t.Fatalf("create with other options: %d %s", rr.Code, rr.Body.String())
	}
	fake.mu.Lock()
	createsAfter := len(fake.createOptionsJSON)
	fake.mu.Unlock()
	if createsAfter != createsBefore+1 {
		t.Fatalf("runner creates = %d, want %d (claim must not create)", createsAfter, createsBefore+1)
	}

	p.reconcile(ctx, time.Now(), false)
	p.wg.Wait()
	if pooled, _ := s.ListPooled(); len(pooled) != 4 {
		t.Fatalf("pooled after refill = %d, want 4", len(pooled))
	}
	p.mu.Lock()
	st := *p.stats[testPoolKey]
	p.mu.Unlock()
	if st.claims != 1 || st.refills != 5 || st.misses != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// An empty pool falls back to a normal create.
func TestCodePoolMissCreates(t *testing.T) {
	fake := &fakeSandboxControl{}
	s := newSweepStore(t)
	reg := newPoolTestRegistry(t, fake, "runner-a")
	cfg := poolTestConfig(1)
	p, err := newCodePool(s, reg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	activeCodePool.Store(p)
	t.Cleanup(func() { activeCodePool.Store(nil) })
	router, err := NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatal(err)
	}
	rr := postCode(t, router, `{"runtime":"n8n-js","slots":2,"taskMemoryMb":256,"reuseWorkers":true}`)
	if rr.Code != http.StatusCreated || strings.Contains(rr.Body.String(), `"pooled"`) {
		t.Fatalf("create on miss: %d %s", rr.Code, rr.Body.String())
	}
	p.mu.Lock()
	misses := p.stats[testPoolKey].misses
	p.mu.Unlock()
	if misses != 1 {
		t.Fatalf("misses = %d, want 1", misses)
	}
}

// Cleanup removes pooled sandboxes that are too old, of a removed pool, on a
// runner that is gone, or over the size. The code idle sweeper leaves pooled
// sandboxes alone.
func TestCodePoolCleanup(t *testing.T) {
	fake := &fakeSandboxControl{}
	s := newSweepStore(t)
	reg := newPoolTestRegistry(t, fake, "runner-a")
	cfg := poolTestConfig(2)
	p, err := newCodePool(s, reg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	created := 0
	p.create = func(_ context.Context, run *registry.Runner, spec *codePoolSpec) (*store.SandboxRecord, error) {
		mu.Lock()
		defer mu.Unlock()
		created++
		rec := &store.SandboxRecord{ID: generateUUID(), Status: "running", CreatedAt: time.Now().Unix(), LastActiveAt: time.Now().Unix(),
			RunnerID: run.ID, TenantID: store.PoolTenantID, Kind: store.KindCode, PoolKey: spec.key}
		return rec, s.Create(rec)
	}
	now := time.Now()
	add := func(key, runner string, created time.Time) string {
		t.Helper()
		id := generateUUID()
		if err := s.Create(&store.SandboxRecord{ID: id, Status: "running", CreatedAt: created.Unix(), LastActiveAt: created.Unix(),
			RunnerID: runner, RunnerControlGRPCAddr: "127.0.0.1:1", TenantID: store.PoolTenantID, Kind: store.KindCode, PoolKey: key}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	tooOld := add(testPoolKey, "runner-a", now.Add(-2*time.Hour))
	removed := add("n8n-js/s9/m256/spare1/reuse1", "runner-a", now.Add(-time.Minute))
	gone := add(testPoolKey, "runner-gone", now.Add(-time.Minute))
	keepOld := add(testPoolKey, "runner-a", now.Add(-50*time.Minute))
	keepNew := add(testPoolKey, "runner-a", now.Add(-40*time.Minute))
	overSize := add(testPoolKey, "runner-a", now.Add(-55*time.Minute))

	// While the manager runs, the code idle sweeper skips pooled rows, also old ones.
	activeCodePool.Store(p)
	t.Cleanup(func() { activeCodePool.Store(nil) })
	sweepIdleCodeSandboxes(context.Background(), s, reg, cfg, runnerControlTLS(cfg), newCodeActivity(), time.Minute, now)
	if pooled, _ := s.ListPooled(); len(pooled) != 6 {
		t.Fatalf("idle sweeper touched pooled rows: %d left, want 6", len(pooled))
	}

	p.reconcile(context.Background(), now, true)
	p.wg.Wait()
	pooled, _ := s.ListPooled()
	ids := map[string]bool{}
	for _, r := range pooled {
		ids[r.ID] = true
	}
	for _, id := range []string{tooOld, removed, gone, overSize} {
		if ids[id] {
			t.Errorf("sandbox %s is still pooled", id)
		}
	}
	if !ids[keepOld] || !ids[keepNew] || len(pooled) != 2 || created != 0 {
		t.Fatalf("pool after cleanup: %v, created %d", ids, created)
	}
}

// A failed refill backs off for that pool.
func TestCodePoolRefillBackoff(t *testing.T) {
	s := newSweepStore(t)
	reg := newPoolTestRegistry(t, &fakeSandboxControl{}, "runner-a")
	p, err := newCodePool(s, reg, poolTestConfig(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	p.create = func(context.Context, *registry.Runner, *codePoolSpec) (*store.SandboxRecord, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil, errors.New("runner is broken")
	}
	ctx := context.Background()
	p.reconcile(ctx, time.Now(), true)
	p.wg.Wait()
	mu.Lock()
	first := calls
	mu.Unlock()
	if first < 1 || first > 3 {
		t.Fatalf("calls in first pass = %d", first)
	}
	p.reconcile(ctx, time.Now(), true)
	p.wg.Wait()
	mu.Lock()
	second := calls
	mu.Unlock()
	if second != first {
		t.Fatalf("calls during backoff = %d, want %d", second, first)
	}
	p.reconcile(ctx, time.Now().Add(time.Minute), true)
	p.wg.Wait()
	mu.Lock()
	third := calls
	mu.Unlock()
	if third == second {
		t.Fatal("no create after the backoff")
	}
}

// Without runners, the manager retries soon and does not back off.
func TestCodePoolNoRunners(t *testing.T) {
	s := newSweepStore(t)
	p, err := newCodePool(s, registry.New(time.Minute), poolTestConfig(2), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.reconcile(context.Background(), time.Now(), true)
	p.wg.Wait()
	p.mu.Lock()
	failures, retryAt := p.failures[testPoolKey], p.retryAt[testPoolKey]
	p.mu.Unlock()
	if failures != 0 || time.Until(retryAt) > 2*time.Second {
		t.Fatalf("failures %d, retry in %s", failures, time.Until(retryAt))
	}
}
