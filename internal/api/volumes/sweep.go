package volumes

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// sweepBatch is how many rows a sweep reads at a time. A var so tests can
// shrink it.
var sweepBatch = 1000

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
// through remove, a batch at a time. A row it can't remove doesn't hold up
// the ones after it. It stops after timing.sweepEvery; the rest waits for the
// next sweep.
func (s *Service) sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timing.sweepEvery)
	defer cancel()
	now := s.now()
	lapsed, err := s.records.FailLapsedSharedVolumes(ctx, now.Add(-s.timing.lease).Unix(), now.Unix())
	switch {
	case err != nil:
		slog.WarnContext(ctx, "shared volume sweep: could not fail the lapsed publishes", "error", err)
	case lapsed > 0:
		slog.InfoContext(ctx, "shared volume sweep: publishes whose lease ran out marked failed", "count", lapsed)
	}
	failedBefore := now.Add(-s.timing.failedGrace).Unix()
	var after store.SharedVolumePosition
	for {
		rows, err := s.records.ListRemovableSharedVolumes(ctx, failedBefore, after, sweepBatch)
		if err != nil {
			slog.WarnContext(ctx, "shared volume sweep: could not list what to remove", "error", err)
			return
		}
		for _, row := range rows {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				slog.WarnContext(ctx, "shared volume sweep: out of time; the rest waits for the next sweep")
			}
			if ctx.Err() != nil {
				return
			}
			s.sweepOne(ctx, row)
		}
		if len(rows) < sweepBatch {
			return
		}
		last := rows[len(rows)-1]
		after = store.SharedVolumePosition{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
}

func (s *Service) sweepOne(ctx context.Context, row *store.SharedVolume) {
	removeCtx, cancel := context.WithTimeout(ctx, s.timing.cleanup)
	defer cancel()
	removed, err := s.remove(removeCtx, row.ID)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "shared volume sweep: removal failed", "version_id", row.ID, "sha256", row.SHA256, "state", row.State, "error", err)
	case removed:
		s.metrics.ObserveSweepRemoval(string(row.State))
		slog.InfoContext(ctx, "shared volume swept", "version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "state", row.State)
	}
}
