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
					now := time.Now()
					if cfg.IdleStopAfter > 0 {
						sweepIdleStopSandboxes(ctx, s, reg, cfg, tlsCfg, now)
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if cfg.IdleDeleteAfter > 0 {
						sweepIdleDeleteSandboxes(ctx, s, reg, cfg, tlsCfg, now)
					}
					return nil
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

func sweepIdleDeleteSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now time.Time) {
	deleteSec := int64(cfg.IdleDeleteAfter.Seconds())
	bufferSec := int64(cfg.IdleDeleteSafetyBuffer.Seconds())
	deleteCutoff := now.Unix() - deleteSec - bufferSec

	records, err := s.ListForIdleReapDelete(deleteCutoff)
	if err != nil {
		slog.Error("idle sweep list delete candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}
		if orphanReapDue(reg, rec.RunnerID, cfg, now) {
			reapOrphanSandbox(s, rec, rec.RunnerID)
			continue
		}
		controlAddr := resolveControlAddr(rec, reg)
		if err := runnerctl.DeleteSandbox(ctx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("idle delete failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		if err := s.Delete(rec.ID); err != nil {
			slog.Error("idle delete store failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		logSandboxDeleted(rec.ID, rec.RunnerID, "idle")
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
		if orphanReapDue(reg, rec.RunnerID, cfg, now) {
			reapOrphanSandbox(s, rec, rec.RunnerID)
			continue
		}
		controlAddr := resolveControlAddr(rec, reg)
		if err := runnerctl.StopSandbox(ctx, controlAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("idle stop failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
			slog.Error("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		logSandboxStopped(rec.ID, rec.RunnerID, "idle")
	}
}
