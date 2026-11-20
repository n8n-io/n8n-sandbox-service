package store

import (
	"context"
	"fmt"
)

func (q sharedVolumeSQL) failLapsedAll(ctx context.Context, lapsedBefore, now int64) (int64, error) {
	result, err := q.db.ExecContext(ctx, q.bind(`UPDATE shared_volumes SET state = 'failed', updated_at = ?, lease_until = 0
		WHERE state = 'publishing' AND lease_until < ?`), now, lapsedBefore)
	if err != nil {
		return 0, fmt.Errorf("store: fail lapsed shared volumes: %w", err)
	}
	failed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: fail lapsed shared volumes: %w", err)
	}
	return failed, nil
}

func (q sharedVolumeSQL) listRemovable(ctx context.Context, failedBefore int64, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	rows, err := q.db.QueryContext(ctx, q.bind(`SELECT `+sharedVolumeCols+` FROM shared_volumes
		WHERE (state = 'deleted' OR (state = 'failed' AND updated_at <= ?)) AND (updated_at, id) > (?, ?)
		ORDER BY updated_at, id LIMIT ?`), failedBefore, after.UpdatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list removable shared volumes: %w", err)
	}
	defer rows.Close()
	var volumes []*SharedVolume
	for rows.Next() {
		volume, err := scanSharedVolume(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list removable shared volumes: %w", err)
		}
		volumes = append(volumes, volume)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list removable shared volumes: %w", err)
	}
	return volumes, nil
}

func (s *SQLiteStore) FailLapsedSharedVolumes(ctx context.Context, lapsedBefore, now int64) (int64, error) {
	return s.sharedVolumes().failLapsedAll(ctx, lapsedBefore, now)
}

func (s *SQLiteStore) ListRemovableSharedVolumes(ctx context.Context, failedBefore int64, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	return s.sharedVolumes().listRemovable(ctx, failedBefore, after, limit)
}

func (s *PostgresStore) FailLapsedSharedVolumes(ctx context.Context, lapsedBefore, now int64) (int64, error) {
	return s.sharedVolumes().failLapsedAll(ctx, lapsedBefore, now)
}

func (s *PostgresStore) ListRemovableSharedVolumes(ctx context.Context, failedBefore int64, after SharedVolumePosition, limit int) ([]*SharedVolume, error) {
	return s.sharedVolumes().listRemovable(ctx, failedBefore, after, limit)
}
