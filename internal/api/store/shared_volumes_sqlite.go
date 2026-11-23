package store

import (
	"context"
	"time"
)

// SQLite ignores VARCHAR lengths, so CHECKs hold the same limits as Postgres.
const sqliteSharedVolumesSchema = `
CREATE TABLE IF NOT EXISTS shared_volumes (
	id                      TEXT    PRIMARY KEY CHECK (length(id) <= 64),
	sha256                  TEXT    NOT NULL CHECK (length(sha256) = 64),
	name                    TEXT    NOT NULL DEFAULT '' CHECK (length(name) <= 128),
	state                   TEXT    NOT NULL CHECK (state IN ('publishing', 'ready', 'failed', 'deleted')),
	tar_bytes               INTEGER NOT NULL DEFAULT 0,
	tar_entries             INTEGER NOT NULL DEFAULT 0,
	tar_content_bytes       INTEGER NOT NULL DEFAULT 0,
	tar_decompressed_bytes  INTEGER NOT NULL DEFAULT 0,
	image_bytes             INTEGER NOT NULL DEFAULT 0,
	image_sha256            TEXT    NOT NULL DEFAULT '' CHECK (length(image_sha256) <= 64),
	published_by            TEXT    NOT NULL CHECK (published_by IN ('admin', 'volume_publisher')),
	created_at              INTEGER NOT NULL,
	ready_at                INTEGER NOT NULL DEFAULT 0,
	updated_at              INTEGER NOT NULL,
	lease_until             INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesSHA256Index + ` ON shared_volumes (sha256) WHERE ` + sharedVolumesLive + `;
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesNameIndex + ` ON shared_volumes (name) WHERE name <> '' AND ` + sharedVolumesLive + `;
CREATE INDEX IF NOT EXISTS shared_volumes_state_idx ON shared_volumes (state, updated_at);
`

func (s *SQLiteStore) sharedVolumes() sharedVolumeSQL {
	return sharedVolumeSQL{newSQLiteDialect(s)}
}

func (s *SQLiteStore) StartSharedVolumePublish(ctx context.Context, volume *SharedVolume, lease time.Duration, maxLiveVolumes int) (*SharedVolume, error) {
	return s.sharedVolumes().startPublish(ctx, volume, lease, maxLiveVolumes)
}

func (s *SQLiteStore) RenewSharedVolumePublish(ctx context.Context, id string, lease time.Duration) (bool, error) {
	return s.sharedVolumes().renewPublish(ctx, id, lease)
}

func (s *SQLiteStore) FinishSharedVolumePublish(ctx context.Context, volume *SharedVolume) (bool, error) {
	return s.sharedVolumes().finishPublish(ctx, volume)
}

func (s *SQLiteStore) FailSharedVolumePublish(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().failPublish(ctx, id)
}

func (s *SQLiteStore) NameReadySharedVolume(ctx context.Context, id, name string) (bool, error) {
	return s.sharedVolumes().setName(ctx, id, name)
}

func (s *SQLiteStore) GetLiveSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().getLive(ctx, sha256)
}

func (s *SQLiteStore) ListReadySharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error) {
	return s.sharedVolumes().listReady(ctx, name)
}

func (s *SQLiteStore) MarkSharedVolumeDeleted(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().markDeleted(ctx, sha256)
}

func (s *SQLiteStore) PurgeSharedVolume(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().purge(ctx, id)
}
