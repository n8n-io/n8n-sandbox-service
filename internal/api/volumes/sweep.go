// This file removes what failed publishes and deletes left in the store.

package volumes

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// sweepBatchRows is how many rows a sweep reads at a time. 1000 rows are less
// than 1 MB, so a read is a small query that uses little memory. A var so
// tests can shrink it.
var sweepBatchRows = 1000

// RunSweeps sweeps every timing.sweepEvery until ctx ends. The first sweep
// starts at a random time, so that the API pods do not all sweep at once. A
// pod sweeps without a lock: each step is a conditional update, or a removal
// that may safely run twice.
func (s *Service) RunSweeps(ctx context.Context) {
	timer := time.NewTimer(rand.N(s.timing.sweepEvery)) //nolint:gosec // jitter that spreads the sweeps of the pods, not a secret
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweepOnce(ctx)
		timer.Reset(s.timing.sweepEvery)
	}
}

// sweepOnce runs one sweep. A row it cannot remove does not stop the rows
// after it. It stops after timing.sweepEvery, and the next sweep removes the
// rest.
func (s *Service) sweepOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.timing.sweepEvery)
	defer cancel()
	failedPublishes, err := s.records.FailLapsedSharedVolumePublishes(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		s.metrics.ObserveSweepError()
		slog.WarnContext(ctx, "shared volume sweep: could not fail the lapsed publishes", "error", err)
	case failedPublishes > 0:
		slog.InfoContext(ctx, "shared volume sweep: publishes whose lease ran out marked failed", "count", failedPublishes)
	}
	if ctx.Err() == nil {
		s.purgeLeftovers(ctx)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.WarnContext(ctx, "shared volume sweep: out of time; the rest waits for the next sweep")
	}
}

// purgeLeftovers purges the deleted rows and the rows failed at least
// timing.failedGrace ago, sweepBatchRows at a time. It stops when ctx ends.
func (s *Service) purgeLeftovers(ctx context.Context) {
	var after store.SharedVolumePosition
	for {
		rows, err := s.records.ListPurgeableSharedVolumes(ctx, s.timing.failedGrace, after, sweepBatchRows)
		if err != nil {
			if ctx.Err() == nil {
				s.metrics.ObserveSweepError()
				slog.WarnContext(ctx, "shared volume sweep: could not list what to remove", "error", err)
			}
			return
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			s.sweepOne(ctx, row)
		}
		if len(rows) < sweepBatchRows {
			return
		}
		last := rows[len(rows)-1]
		after = store.SharedVolumePosition{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
}

func (s *Service) sweepOne(ctx context.Context, row *store.SharedVolume) {
	purgeCtx, cancel := context.WithTimeout(ctx, s.timing.cleanupTimeout)
	defer cancel()
	purged, err := s.purge(purgeCtx, row.ID)
	switch {
	case err != nil && ctx.Err() == nil:
		s.metrics.ObserveSweepError()
		slog.WarnContext(ctx, "shared volume sweep: purge failed", "version_id", row.ID, "sha256", row.SHA256, "state", row.State, "error", err)
	case purged:
		s.metrics.ObserveSweepRemoval(string(row.State))
		slog.InfoContext(ctx, "shared volume swept", "version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "state", row.State)
	}
}
