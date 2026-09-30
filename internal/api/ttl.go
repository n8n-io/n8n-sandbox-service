package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// runnerLifecycleBudget bounds each sweeper RPC: the runner's own budget plus a
// round-trip margin. An abandoned call is retried next sweep; the runner
// finishes it regardless, and stop/delete are idempotent. Var so tests can shrink it.
var runnerLifecycleBudget = runnerruntime.TransitionBudget + time.Minute

// LogIdleSweepConfig logs whether the idle sweeper runs and with which settings.
func LogIdleSweepConfig(cfg *config.APIConfig) {
	if cfg.IdleStopAfter <= 0 && cfg.IdleDeleteAfter <= 0 {
		slog.Info("idle sandbox sweeper disabled")
		return
	}
	slog.Info("idle sandbox sweeper enabled",
		"idle_stop_after", formatIdleDur(cfg.IdleStopAfter),
		"idle_delete_after", formatIdleDur(cfg.IdleDeleteAfter),
		"idle_delete_safety_buffer", cfg.IdleDeleteSafetyBuffer.String(),
		"orphan_reap_buffer", orphanReapBuffer(cfg).String(),
		"sweep_interval", cfg.IdleSweepInterval.String(),
		"sweep_concurrency", sweepConcurrency(cfg))
}

func sweepConcurrency(cfg *config.APIConfig) int {
	if cfg == nil || cfg.IdleSweepConcurrency <= 0 {
		return 1
	}
	return cfg.IdleSweepConcurrency
}

func formatIdleDur(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

func orphanReapBuffer(cfg *config.APIConfig) time.Duration {
	if cfg == nil || cfg.OrphanReapBuffer <= 0 {
		return 5 * time.Minute
	}
	return cfg.OrphanReapBuffer
}

func logSandboxStopped(sandboxID, runnerID, reason string) {
	args := []any{"sandbox_id", sandboxID, "reason", reason}
	if runnerID != "" {
		args = append(args, "runner_id", runnerID)
	}
	slog.Info("sandbox stopped", args...)
}

func logSandboxDeleted(sandboxID, runnerID, reason string) {
	args := []any{"sandbox_id", sandboxID, "reason", reason}
	if runnerID != "" {
		args = append(args, "runner_id", runnerID)
	}
	slog.Info("sandbox deleted", args...)
}

// StartIdleSweeper runs periodic stop/delete for idle sandboxes until ctx is done.
// When sweepLockDB is non-nil (Postgres multi-pod), only the advisory-lock holder runs each sweep.
func StartIdleSweeper(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, sweepLockDB *sql.DB) {
	if cfg.IdleStopAfter <= 0 && cfg.IdleDeleteAfter <= 0 {
		return
	}
	tlsCfg := runnerControlTLS(cfg)
	interval := cfg.IdleSweepInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runSweep := func() error {
					return sweepIdleSandboxes(ctx, s, reg, cfg, tlsCfg, time.Now())
				}

				if sweepLockDB != nil {
					ran, err := store.TryRun(ctx, sweepLockDB, runSweep)
					if err != nil {
						slog.Error("idle sweep failed", "err", err)
					} else if !ran {
						slog.Debug("idle sweep skipped: another pod holds the lock")
					}
				} else {
					_ = runSweep()
				}
			}
		}
	}()
}

// sweepIdleSandboxes runs one pass of every sweep the config enables.
func sweepIdleSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) error {
	if cfg.IdleStopAfter > 0 {
		sweepIdleStopSandboxes(ctx, s, reg, cfg, tlsCfg, now)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cfg.IdleDeleteAfter > 0 {
		sweepIdleDeleteSandboxes(ctx, s, reg, cfg, tlsCfg, now)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if ephemeralIdleWindow(cfg) > 0 {
		sweepEphemeralSandboxes(ctx, s, reg, cfg, tlsCfg, now)
	}
	return nil
}

// ephemeralIdleWindow is how long an ephemeral sandbox may idle before deletion:
// the idle-stop window, or the idle-delete window when idle stop is disabled.
// Both the request-path fence and sweepEphemeralSandboxes use it.
func ephemeralIdleWindow(cfg *config.APIConfig) time.Duration {
	if cfg.IdleStopAfter > 0 {
		return cfg.IdleStopAfter
	}
	return cfg.IdleDeleteAfter
}

func resolveControlAddr(rec *store.SandboxRecord, reg registry.RunnerRegistry) string {
	if rec == nil {
		return ""
	}
	if rec.RunnerID != "" {
		if run, ok := reg.Get(rec.RunnerID); ok {
			if addr := strings.TrimSpace(run.ControlGRPCAddr); addr != "" {
				return addr
			}
		}
	}
	return rec.RunnerControlGRPCAddr
}

func orphanReapDue(reg registry.RunnerRegistry, runnerID string, cfg *config.APIConfig, now time.Time) bool {
	if runnerID == "" {
		return false
	}
	return reg.GoneLongEnough(runnerID, orphanReapBuffer(cfg), now)
}

func reapOrphanSandbox(s store.SandboxStore, rec *store.SandboxRecord, runnerID string) {
	if err := s.Delete(rec.ID); err != nil {
		slog.Error("idle orphan reap store failed", "sandbox_id", rec.ID, "runner_id", runnerID, "err", err)
		return
	}
	logSandboxDeleted(rec.ID, runnerID, "orphan")
}

func withLockedSandbox(ctx context.Context, s store.SandboxStore, id string, fn func(*store.SandboxRecord)) error {
	unlock, err := s.LockSandbox(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	rec, err := s.Get(id)
	if err != nil {
		return err
	}
	if rec != nil {
		fn(rec)
	}
	return nil
}

// sweepAction runs under the sandbox lock with the record re-read. err is a
// runner RPC failure (*runnerCallError) or a store failure.
type sweepAction func(ctx context.Context, rec *store.SandboxRecord) (acted bool, err error)

type sweepStats struct {
	candidates, acted, skippedUnreachable, failed int
}

func runnerKey(rec *store.SandboxRecord) string {
	if rec.RunnerID != "" {
		return rec.RunnerID
	}
	return rec.RunnerControlGRPCAddr
}

func groupByRunner(records []*store.SandboxRecord) map[string][]*store.SandboxRecord {
	groups := make(map[string][]*store.SandboxRecord)
	for _, rec := range records {
		if rec != nil {
			key := runnerKey(rec)
			groups[key] = append(groups[key], rec)
		}
	}
	return groups
}

// runnerCallError marks a runner RPC failure so the sweep can tell it from a
// store failure.
type runnerCallError struct{ err error }

func (e *runnerCallError) Error() string { return e.err.Error() }
func (e *runnerCallError) Unwrap() error { return e.err }

// runnerUnreachable reports whether a runner call could not reach the runner.
// grpc-go returns Unavailable for connect failures, including hosts that
// silently drop packets: its 20s MinConnectTimeout is far below
// runnerLifecycleBudget, so DeadlineExceeded means the call connected and hung.
func runnerUnreachable(err error) bool {
	var rpcErr *runnerCallError
	return errors.As(err, &rpcErr) && status.Code(rpcErr.err) == codes.Unavailable
}

// workPool runs tasks with bounded concurrency. Go blocks while full, so
// callers queue FIFO.
type workPool struct{ g *errgroup.Group }

func newWorkPool(limit int) *workPool {
	g := new(errgroup.Group)
	g.SetLimit(limit)
	return &workPool{g: g}
}

func (w *workPool) Go(fn func()) { w.g.Go(func() error { fn(); return nil }) }

func (w *workPool) Wait() { _ = w.g.Wait() }

// sweepPool runs a phase's candidates with bounded concurrency and skips a
// runner's remaining candidates once one call proves it unreachable.
type sweepPool struct {
	ctx   context.Context
	store store.SandboxStore
	phase string
	act   sweepAction
	pool  *workPool

	mu          sync.Mutex
	unreachable map[string]struct{}
	stats       sweepStats
}

func newSweepPool(ctx context.Context, s store.SandboxStore, phase string, limit, candidates int, act sweepAction) *sweepPool {
	return &sweepPool{
		ctx:         ctx,
		store:       s,
		phase:       phase,
		act:         act,
		pool:        newWorkPool(limit),
		unreachable: make(map[string]struct{}),
		stats:       sweepStats{candidates: candidates},
	}
}

// run executes one candidate under its sandbox lock and records the outcome.
func (p *sweepPool) run(rec *store.SandboxRecord) {
	var acted bool
	var actErr error
	lockErr := withLockedSandbox(p.ctx, p.store, rec.ID, func(fresh *store.SandboxRecord) {
		acted, actErr = p.act(p.ctx, fresh)
	})

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case lockErr != nil:
		if p.ctx.Err() == nil {
			slog.Error("idle sweep lock or refresh failed", "phase", p.phase, "sandbox_id", rec.ID, "err", lockErr)
		}
		p.stats.failed++
	case actErr != nil:
		if runnerUnreachable(actErr) {
			p.unreachable[runnerKey(rec)] = struct{}{}
		}
		p.stats.failed++
	case acted:
		p.stats.acted++
	}
}

// submit runs every runner's candidates and returns once all submitters are
// scheduled.
func (p *sweepPool) submit(groups map[string][]*store.SandboxRecord) {
	var submitters sync.WaitGroup
	for key, group := range groups {
		submitters.Add(1)
		go func(key string, group []*store.SandboxRecord) {
			defer submitters.Done()
			p.submitRunner(key, group)
		}(key, group)
	}
	submitters.Wait()
}

// submitRunner probes the runner with the first candidate and submits the rest
// only if the probe reached it, so a dead runner costs one call per sweep.
func (p *sweepPool) submitRunner(key string, group []*store.SandboxRecord) {
	probed := make(chan struct{})
	p.pool.Go(func() {
		p.run(group[0])
		close(probed)
	})
	<-probed

	rest := group[1:]
	for i, rec := range rest {
		if p.ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		_, down := p.unreachable[key]
		if down {
			p.stats.skippedUnreachable += len(rest) - i
		}
		p.mu.Unlock()
		if down {
			return
		}
		p.pool.Go(func() { p.run(rec) })
	}
}

// wait blocks until every submitted call is done and returns the stats.
func (p *sweepPool) wait() sweepStats {
	p.pool.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// unreachableCount returns the number of runners marked unreachable.
func (p *sweepPool) unreachableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.unreachable)
}

// sweepConcurrently runs act over records with a bounded, runner-aware pool.
func sweepConcurrently(ctx context.Context, s store.SandboxStore, cfg *config.APIConfig, phase string, records []*store.SandboxRecord, act sweepAction) sweepStats {
	start := time.Now()
	p := newSweepPool(ctx, s, phase, sweepConcurrency(cfg), len(records), act)
	p.submit(groupByRunner(records))
	stats := p.wait()

	if stats.candidates > 0 {
		slog.Info("idle sweep phase done",
			"phase", phase,
			"candidates", stats.candidates,
			"acted", stats.acted,
			"skipped_unreachable", stats.skippedUnreachable,
			"failed", stats.failed,
			"unreachable_runners", p.unreachableCount(),
			"elapsed", time.Since(start).String())
	}
	return stats
}

// deleteIdleSandbox deletes rec on the runner, then in the store. Callers hold
// the sandbox lock. Any failure leaves the row in place so the next sweep retries.
func deleteIdleSandbox(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, rec *store.SandboxRecord, now time.Time, reason string) (bool, error) {
	if orphanReapDue(reg, rec.RunnerID, cfg, now) {
		reapOrphanSandbox(s, rec, rec.RunnerID)
		return true, nil
	}
	controlAddr := resolveControlAddr(rec, reg)
	rpcCtx, cancel := context.WithTimeout(ctx, runnerLifecycleBudget)
	err := runnerctl.DeleteSandbox(rpcCtx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("idle delete failed", "sandbox_id", rec.ID, "reason", reason, "err", err)
		}
		return false, &runnerCallError{err}
	}
	if err := s.Delete(rec.ID); err != nil {
		slog.Error("idle delete store failed", "sandbox_id", rec.ID, "reason", reason, "err", err)
		return false, err
	}
	logSandboxDeleted(rec.ID, rec.RunnerID, reason)
	return true, nil
}

// idleSeconds converts an idle window or buffer to whole seconds, rounding up so
// that nothing acts before the configured duration has fully elapsed and a
// sub-second safety buffer does not truncate to none.
func idleSeconds(d time.Duration) int64 {
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++ // ceil without d+time.Second overflowing near math.MaxInt64
	}
	return secs
}

func sweepIdleDeleteSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) sweepStats {
	deleteCutoff := now.Unix() - idleSeconds(cfg.IdleDeleteAfter) - idleSeconds(cfg.IdleDeleteSafetyBuffer)

	records, err := s.ListForIdleReapDelete(deleteCutoff)
	if err != nil {
		slog.Error("idle sweep list delete candidates failed", "err", err)
		return sweepStats{}
	}

	return sweepConcurrently(ctx, s, cfg, "delete", records, func(ctx context.Context, rec *store.SandboxRecord) (bool, error) {
		if rec.Status != "stopped" || rec.LastActiveAt > deleteCutoff {
			return false, nil
		}
		return deleteIdleSandbox(ctx, s, reg, cfg, tlsCfg, rec, now, "idle")
	})
}

// sweepEphemeralSandboxes deletes running ephemeral sandboxes idle past their
// window plus the safety buffer. The request path already refuses them past the
// window, so the fence is up before the irreversible delete.
func sweepEphemeralSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) sweepStats {
	cutoff := now.Unix() - idleSeconds(ephemeralIdleWindow(cfg)) - idleSeconds(cfg.IdleDeleteSafetyBuffer)

	records, err := s.ListForIdleReapStop(cutoff, true)
	if err != nil {
		slog.Error("idle sweep list ephemeral candidates failed", "err", err)
		return sweepStats{}
	}

	return sweepConcurrently(ctx, s, cfg, "ephemeral", records, func(ctx context.Context, rec *store.SandboxRecord) (bool, error) {
		if rec.Status != "running" || rec.LastActiveAt > cutoff {
			return false, nil
		}
		return deleteIdleSandbox(ctx, s, reg, cfg, tlsCfg, rec, now, "ephemeral")
	})
}

func sweepIdleStopSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) sweepStats {
	stopCutoff := now.Unix() - idleSeconds(cfg.IdleStopAfter)

	records, err := s.ListForIdleReapStop(stopCutoff, false)
	if err != nil {
		slog.Error("idle sweep list stop candidates failed", "err", err)
		return sweepStats{}
	}

	return sweepConcurrently(ctx, s, cfg, "stop", records, func(ctx context.Context, rec *store.SandboxRecord) (bool, error) {
		if rec.Status != "running" || rec.LastActiveAt > stopCutoff {
			return false, nil
		}
		if orphanReapDue(reg, rec.RunnerID, cfg, now) {
			reapOrphanSandbox(s, rec, rec.RunnerID)
			return true, nil
		}
		controlAddr := resolveControlAddr(rec, reg)
		rpcCtx, cancel := context.WithTimeout(ctx, runnerLifecycleBudget)
		err := runnerctl.StopSandbox(rpcCtx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("idle stop failed", "sandbox_id", rec.ID, "err", err)
			}
			return false, &runnerCallError{err}
		}
		if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
			slog.Error("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
			return false, err
		}
		logSandboxStopped(rec.ID, rec.RunnerID, "idle")
		return true, nil
	})
}
