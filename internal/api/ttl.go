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

// ephemeralIdleWindow is how long an ephemeral sandbox may sit idle before it
// is deleted: the idle-stop window, since it is deleted where a regular sandbox
// would be stopped, or the idle-delete window when idle stop is disabled. Zero
// when neither is set. The request-path fence (isPastIdleDeleteWindow) and
// sweepEphemeralSandboxes both derive from it, so they cannot disagree.
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

// deleteIdleSandbox removes a sandbox the sweeper has decided is past its
// window. Callers hold the sandbox lock. Any failure leaves the row in place so
// the next sweep retries; the runner delete is idempotent, so a crash between
// the runner delete and the store delete is recovered the same way.
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

// safetyBufferSeconds is IdleDeleteSafetyBuffer in whole seconds, rounded up.
// Timestamps are second-granular, and truncating would shrink the buffer: a
// sub-second setting such as 500ms would silently become no buffer at all,
// letting the delete land in the same second the fence goes up.
func safetyBufferSeconds(cfg *config.APIConfig) int64 {
	return int64((cfg.IdleDeleteSafetyBuffer + time.Second - 1) / time.Second)
}

func sweepIdleDeleteSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) {
	deleteSec := int64(cfg.IdleDeleteAfter.Seconds())
	deleteCutoff := now.Unix() - deleteSec - safetyBufferSeconds(cfg)

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
	windowSec := int64(ephemeralIdleWindow(cfg).Seconds())
	cutoff := now.Unix() - windowSec - safetyBufferSeconds(cfg)

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
	stopSec := int64(cfg.IdleStopAfter.Seconds())
	stopCutoff := now.Unix() - stopSec

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
			if rec.Status != "running" || rec.LastActiveAt > stopCutoff {
				return
			}
			if rec.Ephemeral {
				// Deleted by sweepEphemeralSandboxes, never stopped.
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
