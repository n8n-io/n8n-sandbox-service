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
		slog.Info("idle sandbox sweeper disabled (set SANDBOX_API_IDLE_STOP_AFTER and/or SANDBOX_API_IDLE_DELETE_AFTER to enable)")
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
				sweepIdleSandboxes(context.Background(), s, cfg, tlsCfg)
			}
		}
	}()
}

func sweepIdleSandboxes(ctx context.Context, s *store.Store, cfg *config.APIConfig, tlsCfg *runnerctl.TLS) {
	now := time.Now().Unix()
	var deleteCutoff, stopCutoff int64
	var deleteSec, stopSec, bufferSec int64
	if cfg.IdleDeleteAfter > 0 {
		deleteSec = int64(cfg.IdleDeleteAfter.Seconds())
		bufferSec = int64(cfg.IdleDeleteSafetyBuffer.Seconds())
		deleteCutoff = now - deleteSec - bufferSec
	}
	if cfg.IdleStopAfter > 0 {
		stopSec = int64(cfg.IdleStopAfter.Seconds())
		stopCutoff = now - stopSec
	}

	if cfg.IdleDeleteAfter > 0 {
		records, err := s.ListForIdleReapDelete(deleteCutoff)
		if err != nil {
			slog.Info("idle sweep list delete candidates failed", "err", err)
		} else {
			for _, rec := range records {
				if rec == nil {
					continue
				}
				if err := runnerctl.DeleteSandbox(ctx, rec.RunnerControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
					slog.Info("idle delete failed", "sandbox_id", rec.ID, "err", err)
					continue
				}
				if err := s.Delete(rec.ID); err != nil {
					slog.Info("idle delete store failed", "sandbox_id", rec.ID, "err", err)
				}
			}
		}
	}

	if cfg.IdleStopAfter > 0 {
		records, err := s.ListForIdleReapStop(stopCutoff)
		if err != nil {
			slog.Info("idle sweep list stop candidates failed", "err", err)
		} else {
			for _, rec := range records {
				if rec == nil {
					continue
				}
				if err := runnerctl.StopSandbox(ctx, rec.RunnerControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, rec.ID); err != nil {
					slog.Info("idle stop failed", "sandbox_id", rec.ID, "err", err)
					continue
				}
				if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
					slog.Info("idle stop status update failed", "sandbox_id", rec.ID, "err", err)
				}
			}
		}
	}
}
