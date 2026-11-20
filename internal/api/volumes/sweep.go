package volumes

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// sweepBatch bounds the rows one sweep removes.
const sweepBatch = 1000

// Sweep removes what publishes and deletes left behind, every
// timing.sweepEvery from a random start, until ctx ends. Every API instance
// runs it without a lock: each step is a conditional update, or a removal
// that may run twice.
func (s *Service) Sweep(ctx context.Context) {
	timer := time.NewTimer(rand.N(s.timing.sweepEvery))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweep(ctx)
		timer.Reset(s.timing.sweepEvery)
	}
}

// sweep marks failed the publishes whose lease ran out a lease ago, which
// leaves room for clocks that differ between API instances, then removes the
// deleted rows and the rows failed at least timing.failedGrace ago, each
// through remove. What it can't remove is left for the next sweep.
func (s *Service) sweep(ctx context.Context) {
	now := s.now()
	lapsed, err := s.records.FailLapsedSharedVolumes(ctx, now.Add(-s.timing.lease).Unix(), now.Unix())
	switch {
	case err != nil:
		slog.WarnContext(ctx, "shared volume sweep: could not fail the lapsed publishes", "error", err)
	case lapsed > 0:
		slog.InfoContext(ctx, "shared volume sweep: publishes whose lease ran out marked failed", "count", lapsed)
	}
	rows, err := s.records.ListRemovableSharedVolumes(ctx, now.Add(-s.timing.failedGrace).Unix(), sweepBatch)
	if err != nil {
		slog.WarnContext(ctx, "shared volume sweep: could not list what to remove", "error", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		removeCtx, cancel := context.WithTimeout(ctx, s.timing.cleanup)
		err := s.remove(removeCtx, row.ID)
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "shared volume sweep: removal failed", "version_id", row.ID, "sha256", row.SHA256, "state", row.State, "error", err)
			continue
		}
		s.metrics.ObserveSweepRemoval(string(row.State))
		slog.InfoContext(ctx, "shared volume swept", "version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "state", row.State)
	}
}
