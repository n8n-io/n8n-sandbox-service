package api

import (
	"context"
	"database/sql"
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

// sweepAction runs under the sandbox lock with the record re-read. err is the
// RPC error, so the sweep can tell an unreachable runner from a per-sandbox failure.
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

// grpc-go returns Unavailable for connection failures; the runner maps its own
// failures to Internal, and our deadline surfaces as DeadlineExceeded.
func runnerUnreachable(err error) bool {
	return status.Code(err) == codes.Unavailable
}

// sweepConcurrently runs act over records with a bounded pool. Each runner has
// its own submitter whose first candidate is a probe; the rest are submitted
// only after it returns from a reachable host, so a dead runner costs one dial
// per sweep. Submitters blocked on the pool are admitted FIFO, which spreads
// in-flight calls across runners.
func sweepConcurrently(ctx context.Context, s store.SandboxStore, cfg *config.APIConfig, phase string, records []*store.SandboxRecord, act sweepAction) sweepStats {
	start := time.Now()
	groups := groupByRunner(records)

	var (
		mu          sync.Mutex
		unreachable = make(map[string]struct{})
		stats       sweepStats
	)
	for _, group := range groups {
		stats.candidates += len(group)
	}

	run := func(rec *store.SandboxRecord) {
		var acted bool
		var actErr error
		lockErr := withLockedSandbox(ctx, s, rec.ID, func(fresh *store.SandboxRecord) {
			acted, actErr = act(ctx, fresh)
		})

		mu.Lock()
		defer mu.Unlock()
		switch {
		case lockErr != nil:
			if ctx.Err() == nil {
				slog.Error("idle sweep lock or refresh failed", "phase", phase, "sandbox_id", rec.ID, "err", lockErr)
			}
			stats.failed++
		case actErr != nil:
			if runnerUnreachable(actErr) {
				unreachable[runnerKey(rec)] = struct{}{}
			}
			stats.failed++
		case acted:
			stats.acted++
		}
	}

	pool := new(errgroup.Group)
	pool.SetLimit(sweepConcurrency(cfg))
	var submitters sync.WaitGroup
	for key, group := range groups {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			probed := make(chan struct{})
			pool.Go(func() error {
				run(group[0])
				close(probed)
				return nil
			})
			<-probed
			rest := group[1:]
			for i, rec := range rest {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				_, dead := unreachable[key]
				if dead {
					stats.skippedUnreachable += len(rest) - i
				}
				mu.Unlock()
				if dead {
					return
				}
				pool.Go(func() error {
					run(rec)
					return nil
				})
			}
		}()
	}
	submitters.Wait()
	_ = pool.Wait()

	if stats.candidates > 0 {
		slog.Info("idle sweep phase done",
			"phase", phase,
			"candidates", stats.candidates,
			"acted", stats.acted,
			"skipped_unreachable", stats.skippedUnreachable,
			"failed", stats.failed,
			"unreachable_runners", len(unreachable),
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
		return false, err
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
			return false, err
		}
		if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
			slog.Error("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
			return false, err
		}
		logSandboxStopped(rec.ID, rec.RunnerID, "idle")
		return true, nil
	})
}
