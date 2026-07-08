package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
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
		"sweep_interval", cfg.IdleSweepInterval.String())
}

func formatIdleDur(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

// StartIdleSweeper runs periodic stop/delete for idle sandboxes until ctx is done.
func StartIdleSweeper(ctx context.Context, s *store.Store, cfg *config.APIConfig) {
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
				// Use the same ctx as the ticker loop so stop/delete RPCs honor
				// sweepCancel() from shutdown instead of blocking on Background.
				now := time.Now().Unix()
				if cfg.IdleStopAfter > 0 {
					sweepIdleStopSandboxes(ctx, s, cfg, tlsCfg, now)
				}
				if ctx.Err() != nil {
					return
				}
				if cfg.IdleDeleteAfter > 0 {
					sweepIdleDeleteSandboxes(ctx, s, cfg, tlsCfg, now)
				}
			}
		}
	}()
}

func sweepIdleDeleteSandboxes(ctx context.Context, s *store.Store, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now int64) {
	deleteSec := int64(cfg.IdleDeleteAfter.Seconds())
	bufferSec := int64(cfg.IdleDeleteSafetyBuffer.Seconds())
	deleteCutoff := now - deleteSec - bufferSec

	records, err := s.ListForIdleReapDelete(deleteCutoff)
	if err != nil {
		slog.Error("idle sweep list delete candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}
		if err := runnerctl.DeleteSandbox(ctx, rec.RunnerControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("idle delete failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		if err := s.Delete(rec.ID); err != nil {
			slog.Error("idle delete store failed", "sandbox_id", rec.ID, "err", err)
		}
	}
}

func sweepIdleStopSandboxes(ctx context.Context, s *store.Store, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, now int64) {
	stopSec := int64(cfg.IdleStopAfter.Seconds())
	stopCutoff := now - stopSec

	records, err := s.ListForIdleReapStop(stopCutoff)
	if err != nil {
		slog.Error("idle sweep list stop candidates failed", "err", err)
		return
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}
		if err := runnerctl.StopSandbox(ctx, rec.RunnerControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("idle stop failed", "sandbox_id", rec.ID, "err", err)
			continue
		}
		if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
			slog.Error("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
		}
	}
}
