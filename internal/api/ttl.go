package api

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

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
		"sweep_interval", cfg.IdleSweepInterval.String())
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

// deleteIdleSandbox deletes rec on the runner, then in the store. Callers hold
// the sandbox lock. Any failure leaves the row in place so the next sweep retries.
func deleteIdleSandbox(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, rec *store.SandboxRecord, now time.Time, reason string) {
	if orphanReapDue(reg, rec.RunnerID, cfg, now) {
		reapOrphanSandbox(s, rec, rec.RunnerID)
		return
	}
	controlAddr := resolveControlAddr(rec, reg)
	if err := runnerctl.DeleteSandbox(ctx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
		if ctx.Err() == nil {
			slog.Error("idle delete failed", "sandbox_id", rec.ID, "reason", reason, "err", err)
		}
		return
	}
	if err := s.Delete(rec.ID); err != nil {
		slog.Error("idle delete store failed", "sandbox_id", rec.ID, "reason", reason, "err", err)
		return
	}
	logSandboxDeleted(rec.ID, rec.RunnerID, reason)
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

func sweepIdleDeleteSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) {
	deleteCutoff := now.Unix() - idleSeconds(cfg.IdleDeleteAfter) - idleSeconds(cfg.IdleDeleteSafetyBuffer)

	records, err := s.ListForIdleReapDelete(deleteCutoff)
	if err != nil {
		slog.Error("idle sweep list delete candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}
		id := rec.ID
		err := withLockedSandbox(ctx, s, id, func(rec *store.SandboxRecord) {
			if rec.Status != "stopped" || rec.LastActiveAt > deleteCutoff {
				return
			}
			deleteIdleSandbox(ctx, s, reg, cfg, tlsCfg, rec, now, "idle")
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("idle delete lock or refresh failed", "sandbox_id", id, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// sweepEphemeralSandboxes deletes running ephemeral sandboxes idle past their
// window plus the safety buffer. The request path already refuses them past the
// window, so the fence is up before the irreversible delete.
func sweepEphemeralSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) {
	cutoff := now.Unix() - idleSeconds(ephemeralIdleWindow(cfg)) - idleSeconds(cfg.IdleDeleteSafetyBuffer)

	// Running rows idle past cutoff; the ephemeral ones are filtered here.
	records, err := s.ListForIdleReapStop(cutoff)
	if err != nil {
		slog.Error("idle sweep list ephemeral candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil || !rec.Ephemeral {
			continue
		}
		id := rec.ID
		err := withLockedSandbox(ctx, s, id, func(rec *store.SandboxRecord) {
			if !rec.Ephemeral || rec.Status != "running" || rec.LastActiveAt > cutoff {
				return
			}
			deleteIdleSandbox(ctx, s, reg, cfg, tlsCfg, rec, now, "ephemeral")
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("ephemeral delete lock or refresh failed", "sandbox_id", id, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func sweepIdleStopSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) {
	stopCutoff := now.Unix() - idleSeconds(cfg.IdleStopAfter)

	records, err := s.ListForIdleReapStop(stopCutoff)
	if err != nil {
		slog.Error("idle sweep list stop candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}
		id := rec.ID
		err := withLockedSandbox(ctx, s, id, func(rec *store.SandboxRecord) {
			// Ephemeral rows are deleted by sweepEphemeralSandboxes, never stopped.
			if rec.Status != "running" || rec.Ephemeral || rec.LastActiveAt > stopCutoff {
				return
			}
			if orphanReapDue(reg, rec.RunnerID, cfg, now) {
				reapOrphanSandbox(s, rec, rec.RunnerID)
				return
			}
			controlAddr := resolveControlAddr(rec, reg)
			if err := runnerctl.StopSandbox(ctx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
				if ctx.Err() == nil {
					slog.Error("idle stop failed", "sandbox_id", rec.ID, "err", err)
				}
				return
			}
			if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
				slog.Error("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
				return
			}
			logSandboxStopped(rec.ID, rec.RunnerID, "idle")
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("idle stop lock or refresh failed", "sandbox_id", id, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}
