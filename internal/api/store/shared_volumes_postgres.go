package store

import (
	"context"
	"time"
)

// postgresSharedVolumesSchema runs one statement per Exec. In one
// transaction, the index statements' SHARE locks and the comments' SHARE
// UPDATE EXCLUSIVE lock deadlock two API pods that start at the same time.
var postgresSharedVolumesSchema = []string{
	`CREATE TABLE IF NOT EXISTS shared_volumes (
	id                      VARCHAR(64)  PRIMARY KEY,
	sha256                  VARCHAR(64)  NOT NULL CONSTRAINT shared_volumes_sha256_check CHECK (length(sha256) = 64),
	name                    VARCHAR(128) NOT NULL DEFAULT '',
	state                   VARCHAR(16)  NOT NULL CONSTRAINT shared_volumes_state_check CHECK (state IN ('publishing', 'ready', 'failed', 'deleted')),
	tar_bytes               BIGINT       NOT NULL DEFAULT 0,
	tar_entries             BIGINT       NOT NULL DEFAULT 0,
	tar_content_bytes       BIGINT       NOT NULL DEFAULT 0,
	tar_decompressed_bytes  BIGINT       NOT NULL DEFAULT 0,
	image_bytes             BIGINT       NOT NULL DEFAULT 0,
	image_sha256            VARCHAR(64)  NOT NULL DEFAULT '',
	published_by            VARCHAR(16)  NOT NULL CONSTRAINT shared_volumes_published_by_check CHECK (published_by IN ('admin', 'volume_publisher')),
	created_at              BIGINT       NOT NULL,
	ready_at                BIGINT       NOT NULL DEFAULT 0,
	updated_at              BIGINT       NOT NULL,
	lease_until             BIGINT       NOT NULL DEFAULT 0
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesSHA256Index + ` ON shared_volumes (sha256) WHERE ` + sharedVolumesLive,
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesNameIndex + ` ON shared_volumes (name) WHERE name <> '' AND ` + sharedVolumesLive,
	`CREATE INDEX IF NOT EXISTS shared_volumes_state_idx ON shared_volumes (state, updated_at)`,
	`COMMENT ON TABLE shared_volumes IS 'Shared volumes: one row per published tar, written before its objects in the volume store and purged after them';
COMMENT ON COLUMN shared_volumes.id IS 'Chosen by the API; names the volume''s objects in the volume store';
COMMENT ON COLUMN shared_volumes.sha256 IS 'sha256 of the tar, lowercase hex';
COMMENT ON COLUMN shared_volumes.name IS 'Name tenants attach by; empty for none. Unique among publishing and ready rows';
COMMENT ON COLUMN shared_volumes.state IS 'publishing, ready, failed (a publish that did not finish) or deleted (purged with its objects)';
COMMENT ON COLUMN shared_volumes.tar_bytes IS 'Length of the tar as published, plain or gzip';
COMMENT ON COLUMN shared_volumes.tar_entries IS 'Entries of the tar, with the directories it leaves out';
COMMENT ON COLUMN shared_volumes.tar_content_bytes IS 'Sum of the sizes of the regular files in the tar';
COMMENT ON COLUMN shared_volumes.tar_decompressed_bytes IS 'Length of the tar once decompressed';
COMMENT ON COLUMN shared_volumes.image_bytes IS 'Size of the ext4 image; 0 for none';
COMMENT ON COLUMN shared_volumes.published_by IS 'Role of the key that published it: admin or volume_publisher';
COMMENT ON COLUMN shared_volumes.updated_at IS 'Unix seconds, by the database clock, when the state last changed';
COMMENT ON COLUMN shared_volumes.lease_until IS 'Unix seconds, by the database clock, when the lease of a publishing row ends; another publish may then take it over'`,
}

func (s *PostgresStore) sharedVolumes() sharedVolumeSQL {
	return sharedVolumeSQL{newPostgresDialect(s)}
}

func (s *PostgresStore) StartSharedVolumePublish(ctx context.Context, volume *SharedVolume, lease time.Duration, maxLiveVolumes int) (*SharedVolume, error) {
	return s.sharedVolumes().startPublish(ctx, volume, lease, maxLiveVolumes)
}

func (s *PostgresStore) RenewSharedVolumePublish(ctx context.Context, id string, lease time.Duration) (bool, error) {
	return s.sharedVolumes().renewPublish(ctx, id, lease)
}

func (s *PostgresStore) FinishSharedVolumePublish(ctx context.Context, volume *SharedVolume) (bool, error) {
	return s.sharedVolumes().finishPublish(ctx, volume)
}

func (s *PostgresStore) FailSharedVolumePublish(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().failPublish(ctx, id)
}

func (s *PostgresStore) NameReadySharedVolume(ctx context.Context, id, name string) (string, error) {
	return s.sharedVolumes().setName(ctx, id, name)
}

func (s *PostgresStore) GetLiveSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().getLive(ctx, sha256)
}

func (s *PostgresStore) ListReadySharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error) {
	return s.sharedVolumes().listReady(ctx, name)
}

func (s *PostgresStore) MarkSharedVolumeDeleted(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().markDeleted(ctx, sha256)
}

func (s *PostgresStore) PurgeSharedVolume(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().purge(ctx, id)
}
