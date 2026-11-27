// This file finds the shared volume rows that failed publishes and deletes
// left behind.

package store

import (
	"context"
	"fmt"
	"time"
)

func (q sharedVolumeSQL) failAllLapsedPublishes(ctx context.Context) (int64, error) {
	result, err := q.db.ExecContext(ctx, q.bind(`UPDATE shared_volumes SET state = 'failed', updated_at = `+q.nowSQL+`, lease_until = 0
		WHERE state = 'publishing' AND lease_until < `+q.nowSQL))
	if err != nil {
		return 0, fmt.Errorf("store: fail lapsed shared volume publishes: %w", err)
	}
	failedRows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: fail lapsed shared volume publishes: %w", err)
	}
	return failedRows, nil
}

func (q sharedVolumeSQL) listPurgeable(ctx context.Context, minAge time.Duration, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	rows, err := q.db.QueryContext(ctx, q.bind(`SELECT `+sharedVolumeCols+` FROM shared_volumes
		WHERE state IN ('failed', 'deleted') AND updated_at <= `+q.nowSQL+` - ? AND (updated_at, id) > (?, ?)
		ORDER BY updated_at, id LIMIT ?`), int64(minAge/time.Second), after.UpdatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list purgeable shared volumes: %w", err)
	}
	defer rows.Close()
	var volumes []*SharedVolume
	for rows.Next() {
		volume, err := scanSharedVolume(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list purgeable shared volumes: %w", err)
		}
		volumes = append(volumes, volume)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list purgeable shared volumes: %w", err)
	}
	return volumes, nil
}

func (s *SQLiteStore) FailLapsedSharedVolumePublishes(ctx context.Context) (int64, error) {
	return s.sharedVolumes().failAllLapsedPublishes(ctx)
}

func (s *SQLiteStore) ListPurgeableSharedVolumes(ctx context.Context, minAge time.Duration, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	return s.sharedVolumes().listPurgeable(ctx, minAge, after, limit)
}

func (s *PostgresStore) FailLapsedSharedVolumePublishes(ctx context.Context) (int64, error) {
	return s.sharedVolumes().failAllLapsedPublishes(ctx)
}

func (s *PostgresStore) ListPurgeableSharedVolumes(ctx context.Context, minAge time.Duration, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	return s.sharedVolumes().listPurgeable(ctx, minAge, after, limit)
}
