package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// codePoolKey is the text form of a code sandbox option set. A pool holds
// sandboxes of one key only, because the daemon settings are fixed at start.
func codePoolKey(o runnerruntime.CreateOptions) string {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("%s/s%d/m%d/spare%d/reuse%d", o.Runtime, o.Slots, o.TaskMemoryMB, b(o.SpareWorkers), b(o.ReuseWorkers))
}

// codeCreateOptions applies the defaults of POST /v1/code/sandboxes and
// validates the result.
func codeCreateOptions(runtime string, slots, taskMemoryMB int, spare *bool, reuse bool) (runnerruntime.CreateOptions, error) {
	if runtime == "" {
		runtime = defaultCodeRuntime
	}
	if slots == 0 {
		slots = defaultCodeSlots
	}
	if taskMemoryMB == 0 {
		taskMemoryMB = defaultCodeTaskMemoryMB
	}
	spareWorkers := true
	if spare != nil {
		spareWorkers = *spare
	}
	opts := runnerruntime.CreateOptions{
		Egress:       runnerruntime.EgressNone,
		Kind:         runnerruntime.KindCode,
		Runtime:      runtime,
		Slots:        slots,
		TaskMemoryMB: taskMemoryMB,
		SpareWorkers: spareWorkers,
		ReuseWorkers: reuse,
	}
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return opts, err
	}
	if _, err := runnerruntime.ParseCreateOptions(string(optsJSON)); err != nil {
		return opts, err
	}
	return opts, nil
}

// codeCreateError carries the HTTP status for a failed code sandbox create.
type codeCreateError struct {
	status int
	msg    string
	err    error
}

func (e *codeCreateError) Error() string { return e.msg }
func (e *codeCreateError) Unwrap() error { return e.err }

// createCodeSandboxOn creates a code sandbox on run and stores its row. The
// request handler and the pool manager both use it. It returns the record and
// the time of the runner create RPC in milliseconds.
func createCodeSandboxOn(ctx context.Context, s store.SandboxStore, cfg *config.APIConfig, run *registry.Runner, requested runnerruntime.CreateOptions, tenantID, poolKey string) (*store.SandboxRecord, int64, error) {
	optsJSON, err := json.Marshal(requested)
	if err != nil {
		return nil, 0, &codeCreateError{status: http.StatusInternalServerError, msg: err.Error(), err: err}
	}
	sandboxID := generateUUID()
	tlsCfg := runnerControlTLS(cfg)

	tRunner := time.Now()
	createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), runnerCreateBudget)
	defer cancelCreate()
	gresp, err := runnerctl.CreateSandbox(createCtx, run.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID, string(optsJSON))
	runnerCreateMs := time.Since(tRunner).Milliseconds()
	if err != nil {
		slog.ErrorContext(ctx, "create code sandbox failed: runner control create", "sandbox_id", sandboxID, "runner_id", run.ID, "error", err)
		return nil, runnerCreateMs, &codeCreateError{status: http.StatusInternalServerError, msg: "failed to create container: " + err.Error(), err: err}
	}
	rollback := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runnerCreateBudget)
		defer cancel()
		if err := runnerctl.DeleteSandbox(ctx, run.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID); err != nil {
			slog.ErrorContext(ctx, "create code sandbox failed: compensating runner delete", "sandbox_id", sandboxID, "error", err)
		}
	}
	applied, err := runnerruntime.ParseCreateOptions(gresp.GetAppliedCreateOptionsJson())
	if err != nil || *applied != requested {
		rollback()
		return nil, runnerCreateMs, &codeCreateError{status: http.StatusBadGateway, msg: "runner did not apply the requested code sandbox options"}
	}

	now := time.Now().Unix()
	record := &store.SandboxRecord{
		ID:                    sandboxID,
		Status:                "running",
		CreatedAt:             now,
		LastActiveAt:          now,
		ContainerIP:           gresp.GetContainerIp(),
		DaemonPort:            8081,
		RunnerID:              run.ID,
		RunnerHTTPBase:        strings.TrimRight(run.HTTPBaseURL, "/"),
		RunnerControlGRPCAddr: run.ControlGRPCAddr,
		TenantID:              tenantID,
		// Ephemeral: never stopped. The code idle sweeper deletes it.
		Ephemeral: true,
		Egress:    string(runnerruntime.EgressNone),
		Kind:      store.KindCode,
		PoolKey:   poolKey,
	}
	if err := s.Create(record); err != nil {
		rollback()
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrTenantNotFound) {
			status = http.StatusUnauthorized
		}
		return nil, runnerCreateMs, &codeCreateError{status: status, msg: "failed to store sandbox: " + err.Error(), err: err}
	}
	return record, runnerCreateMs, nil
}

// activeCodePool is the pool manager of the process, or nil without pools.
// It is global like codeActivityTracker: the spike runs one API pod.
var activeCodePool atomic.Pointer[codePool]

type codePoolSpec struct {
	key  string
	opts runnerruntime.CreateOptions
	size int
}

type codePoolStats struct {
	claims, misses, refills, refillFailures, retired int64
}

type runnerPlacement struct {
	lastSeen time.Time
	placed   int32
}

// codePool keeps warm pools of code sandboxes. Pooled sandboxes are store rows
// owned by store.PoolTenantID. A create request claims one (claim); the
// manager refills each pool to its size in the background.
type codePool struct {
	s       store.SandboxStore
	reg     registry.RunnerRegistry
	cfg     *config.APIConfig
	client  *http.Client
	lockDB  *sql.DB
	specs   map[string]*codePoolSpec
	order   []string
	wake    chan struct{}
	sem     chan struct{}
	backoff func(failures int) time.Duration
	// create makes one pooled sandbox. Tests replace it.
	create func(ctx context.Context, run *registry.Runner, spec *codePoolSpec) (*store.SandboxRecord, error)

	mu       sync.Mutex
	inFlight map[string]int
	failures map[string]int
	retryAt  map[string]time.Time
	placed   map[string]*runnerPlacement
	versions map[string]string
	stats    map[string]*codePoolStats
	wg       sync.WaitGroup
}

func newCodePool(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, lockDB *sql.DB) (*codePool, error) {
	p := &codePool{
		s: s, reg: reg, cfg: cfg, lockDB: lockDB,
		specs:    make(map[string]*codePoolSpec),
		wake:     make(chan struct{}, 1),
		sem:      make(chan struct{}, max(1, cfg.CodePoolRefillConcurrency)),
		inFlight: make(map[string]int),
		failures: make(map[string]int),
		retryAt:  make(map[string]time.Time),
		placed:   make(map[string]*runnerPlacement),
		versions: make(map[string]string),
		stats:    make(map[string]*codePoolStats),
		backoff: func(failures int) time.Duration {
			return min(time.Second<<min(failures-1, 5), 30*time.Second)
		},
	}
	transport, err := newRunnerTransport(cfg)
	if err != nil {
		return nil, err
	}
	p.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	p.create = p.createPooled
	for i, pc := range cfg.CodePools {
		opts, err := codeCreateOptions(pc.Runtime, pc.Slots, pc.TaskMemoryMB, pc.SpareWorkers, pc.ReuseWorkers)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_CODE_POOLS[%d]: %w", i, err)
		}
		key := codePoolKey(opts)
		if _, dup := p.specs[key]; dup {
			return nil, fmt.Errorf("SANDBOX_CODE_POOLS[%d]: duplicate pool %s", i, key)
		}
		p.specs[key] = &codePoolSpec{key: key, opts: opts, size: pc.Size}
		p.order = append(p.order, key)
		p.stats[key] = &codePoolStats{}
	}
	return p, nil
}

// StartCodePool starts the pool manager when cfg.CodePools is set. lockDB is
// the Postgres handle for the leader lock, or nil (SQLite, one pod).
func StartCodePool(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, lockDB *sql.DB) error {
	if len(cfg.CodePools) == 0 {
		slog.Info("code sandbox pool disabled")
		return nil
	}
	p, err := newCodePool(s, reg, cfg, lockDB)
	if err != nil {
		return err
	}
	activeCodePool.Store(p)
	for _, key := range p.order {
		slog.Info("code sandbox pool enabled", "pool", key, "size", p.specs[key].size)
	}
	go p.run(ctx)
	return nil
}

func (p *codePool) run(ctx context.Context) {
	interval := p.cfg.CodePoolCheckInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	statsT := time.NewTicker(time.Minute)
	defer statsT.Stop()
	p.tick(ctx, time.Now(), true)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			p.tick(ctx, now, true)
		case <-p.wake:
			p.tick(ctx, time.Now(), false)
		case <-statsT.C:
			p.logStats()
		}
	}
}

func (p *codePool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// claim takes a pooled sandbox for opts and gives it to tenantID. It returns
// nil when no pool has this option set or the pool is empty.
func (p *codePool) claim(opts runnerruntime.CreateOptions, tenantID string) (*store.SandboxRecord, error) {
	key := codePoolKey(opts)
	spec, ok := p.specs[key]
	if !ok || spec.size == 0 {
		return nil, nil
	}
	rec, err := p.s.ClaimPooled(key, tenantID, time.Now().Unix())
	p.mu.Lock()
	if err == nil && rec != nil {
		p.stats[key].claims++
	} else if err == nil {
		p.stats[key].misses++
	}
	p.mu.Unlock()
	if d := p.cfg.CodePoolRefillDelay; d > 0 {
		time.AfterFunc(d, p.signal)
	} else {
		p.signal()
	}
	return rec, err
}

// takeVersion returns the runtime version that the manager read when it
// created the sandbox, and forgets it.
func (p *codePool) takeVersion(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.versions[id]
	delete(p.versions, id)
	return v
}

// tick removes pooled sandboxes that must go (cleanup only), and starts the
// creates that bring each pool back to its size.
func (p *codePool) tick(ctx context.Context, now time.Time, cleanup bool) {
	work := func() error {
		p.reconcile(ctx, now, cleanup)
		return nil
	}
	if p.lockDB != nil {
		if _, err := store.TryRunKey(ctx, p.lockDB, store.CodePoolLockKey, work); err != nil {
			slog.Error("code pool: leader lock", "error", err)
		}
		return
	}
	_ = work()
}

func (p *codePool) reconcile(ctx context.Context, now time.Time, cleanup bool) {
	rows, err := p.s.ListPooled()
	if err != nil {
		slog.Error("code pool: list pooled sandboxes", "error", err)
		return
	}
	byKey := make(map[string][]*store.SandboxRecord)
	for _, rec := range rows {
		if cleanup {
			reason := ""
			if _, ok := p.specs[rec.PoolKey]; !ok {
				reason = "pool_removed"
			} else if p.cfg.CodePoolMaxAge > 0 && now.Unix()-rec.CreatedAt >= idleSeconds(p.cfg.CodePoolMaxAge) {
				reason = "pool_max_age"
			} else if _, ok := p.reg.Get(rec.RunnerID); !ok {
				reason = "pool_runner_gone"
			}
			if reason != "" {
				p.retire(ctx, rec, now, reason)
				continue
			}
		}
		byKey[rec.PoolKey] = append(byKey[rec.PoolKey], rec)
	}
	for _, key := range p.order {
		spec := p.specs[key]
		have := byKey[key]
		if cleanup && len(have) > spec.size {
			// Oldest first: ListPooled orders by created_at.
			for _, rec := range have[:len(have)-spec.size] {
				p.retire(ctx, rec, now, "pool_over_size")
			}
			have = have[len(have)-spec.size:]
		}
		p.mu.Lock()
		missing := spec.size - len(have) - p.inFlight[key]
		if now.Before(p.retryAt[key]) {
			missing = 0
		}
		if missing > 0 {
			p.inFlight[key] += missing
		}
		p.mu.Unlock()
		for range missing {
			p.wg.Add(1)
			go p.refill(ctx, spec, now)
		}
	}
}

func (p *codePool) refill(ctx context.Context, spec *codePoolSpec, now time.Time) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		p.inFlight[spec.key]--
		p.mu.Unlock()
	}()
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-p.sem }()
	// A refill of the same tick that failed sets the backoff for the others.
	p.mu.Lock()
	skip := now.Before(p.retryAt[spec.key])
	p.mu.Unlock()
	if skip {
		return
	}

	t0 := time.Now()
	run, err := p.pickRunner()
	var rec *store.SandboxRecord
	if err == nil {
		rec, err = p.create(ctx, run, spec)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if errors.Is(err, registry.ErrNoRunners) {
		// Runners register a few seconds after an API start. That is not a
		// failure of the pool, so retry soon and without backoff.
		p.retryAt[spec.key] = time.Now().Add(time.Second)
		time.AfterFunc(time.Second, p.signal)
		slog.Debug("code pool: no runner for refill", "pool", spec.key)
		return
	}
	if err != nil {
		p.failures[spec.key]++
		wait := p.backoff(p.failures[spec.key])
		p.retryAt[spec.key] = time.Now().Add(wait)
		p.stats[spec.key].refillFailures++
		// Try again when the backoff ends, not only at the next tick.
		time.AfterFunc(wait, p.signal)
		if ctx.Err() == nil {
			slog.Error("code pool: refill failed", "pool", spec.key, "retry_in", wait.String(), "error", err)
		}
		return
	}
	p.failures[spec.key] = 0
	p.stats[spec.key].refills++
	slog.Debug("code pool: refilled", "pool", spec.key, "sandbox_id", rec.ID, "runner_id", rec.RunnerID, "ms", time.Since(t0).Milliseconds())
}

// pickRunner picks the eligible runner with the lowest container count. The
// count comes from heartbeats (up to 10 s old), so the manager adds the
// sandboxes that it placed on a runner since that runner's last heartbeat.
// Without this, a refill burst puts the whole pool on one runner.
func (p *codePool) pickRunner() (*registry.Runner, error) {
	runners, err := p.reg.Eligible()
	if err != nil {
		return nil, err
	}
	if len(runners) == 0 {
		return nil, registry.ErrNoRunners
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *registry.Runner
	var bestScore int32
	for i := range runners {
		run := &runners[i]
		pl := p.placed[run.ID]
		if pl == nil || !pl.lastSeen.Equal(run.LastSeen) {
			pl = &runnerPlacement{lastSeen: run.LastSeen}
			p.placed[run.ID] = pl
		}
		score := run.CapacityUsed + pl.placed
		if best == nil || score < bestScore || (score == bestScore && run.ID < best.ID) {
			best, bestScore = run, score
		}
	}
	p.placed[best.ID].placed++
	return best, nil
}

func (p *codePool) createPooled(ctx context.Context, run *registry.Runner, spec *codePoolSpec) (*store.SandboxRecord, error) {
	rec, _, err := createCodeSandboxOn(ctx, p.s, p.cfg, run, spec.opts, store.PoolTenantID, spec.key)
	if err != nil {
		return nil, err
	}
	if st, err := readCodeDaemonState(ctx, p.client, p.cfg, rec, "healthz"); err == nil {
		p.mu.Lock()
		p.versions[rec.ID] = st.RuntimeVersion
		p.mu.Unlock()
	}
	return rec, nil
}

// retire takes rec out of the pool, then deletes it. If a claim took rec
// first, it does nothing. If the delete fails, the row stays as an admin-owned
// code sandbox, and the code idle sweeper deletes it later.
func (p *codePool) retire(ctx context.Context, rec *store.SandboxRecord, now time.Time, reason string) {
	ok, err := p.s.UnpoolSandbox(rec.ID)
	if err != nil {
		slog.Error("code pool: unpool", "sandbox_id", rec.ID, "error", err)
		return
	}
	if !ok {
		return
	}
	p.mu.Lock()
	delete(p.versions, rec.ID)
	if st := p.stats[rec.PoolKey]; st != nil {
		st.retired++
	}
	p.mu.Unlock()
	tlsCfg := runnerControlTLS(p.cfg)
	_ = withLockedSandbox(ctx, p.s, rec.ID, func(cur *store.SandboxRecord) {
		_, _ = deleteIdleSandbox(ctx, p.s, p.reg, p.cfg, tlsCfg, cur, now, reason)
	})
}

func (p *codePool) logStats() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range p.order {
		st := p.stats[key]
		slog.Info("code pool stats", "pool", key, "size", p.specs[key].size, "in_flight", p.inFlight[key],
			"claims", st.claims, "misses", st.misses, "refills", st.refills,
			"refill_failures", st.refillFailures, "retired", st.retired)
	}
}
