package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

const postgresSharedVolumesSchema = `
CREATE TABLE IF NOT EXISTS shared_volumes (
	id                  VARCHAR(64)  PRIMARY KEY,
	sha256              CHAR(64)     NOT NULL,
	name                VARCHAR(128) NOT NULL DEFAULT '',
	state               VARCHAR(16)  NOT NULL CONSTRAINT shared_volumes_state_check CHECK (state IN ('publishing', 'ready', 'failed', 'deleted')),
	tar_bytes           BIGINT       NOT NULL DEFAULT 0,
	entries             BIGINT       NOT NULL DEFAULT 0,
	content_bytes       BIGINT       NOT NULL DEFAULT 0,
	decompressed_bytes  BIGINT       NOT NULL DEFAULT 0,
	image_bytes         BIGINT       NOT NULL DEFAULT 0,
	image_sha256        VARCHAR(64)  NOT NULL DEFAULT '',
	published_by        VARCHAR(16)  NOT NULL CONSTRAINT shared_volumes_published_by_check CHECK (published_by IN ('admin', 'publisher')),
	created_at          BIGINT       NOT NULL,
	ready_at            BIGINT       NOT NULL DEFAULT 0,
	updated_at          BIGINT       NOT NULL,
	lease_until         BIGINT       NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesSHA256Index + ` ON shared_volumes (sha256) WHERE ` + sharedVolumesLive + `;
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesNameIndex + ` ON shared_volumes (name) WHERE name <> '' AND ` + sharedVolumesLive + `;
CREATE INDEX IF NOT EXISTS shared_volumes_state_idx ON shared_volumes (state, updated_at);
COMMENT ON TABLE shared_volumes IS 'Shared volumes: one row per published tar, written before its objects in the volume store and removed after them';
COMMENT ON COLUMN shared_volumes.id IS 'Chosen by the API; names the volume''s objects in the volume store';
COMMENT ON COLUMN shared_volumes.sha256 IS 'sha256 of the tar, lowercase hex';
COMMENT ON COLUMN shared_volumes.name IS 'Name tenants attach by; empty for none. Unique among publishing and ready rows';
COMMENT ON COLUMN shared_volumes.state IS 'publishing, ready, failed (a publish that did not finish) or deleted (removed with its objects)';
COMMENT ON COLUMN shared_volumes.entries IS 'Entries of the tar, with the directories it leaves out';
COMMENT ON COLUMN shared_volumes.content_bytes IS 'Sum of the sizes of the regular files in the tar';
COMMENT ON COLUMN shared_volumes.decompressed_bytes IS 'Length of the tar once decompressed';
COMMENT ON COLUMN shared_volumes.image_bytes IS 'Size of the ext4 image; 0 for none';
COMMENT ON COLUMN shared_volumes.published_by IS 'Role of the key that published it: admin or publisher';
COMMENT ON COLUMN shared_volumes.updated_at IS 'Unix seconds when the state last changed';
COMMENT ON COLUMN shared_volumes.lease_until IS 'Unix seconds when the lease of a publishing row ends; another publish may then take it over';
`

func (s *PostgresStore) sharedVolumes() sharedVolumeSQL {
	return sharedVolumeSQL{
		db: s.db,
		begin: func(ctx context.Context) (*sqlTx, error) {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return nil, err
			}
			return &sqlTx{
				sqlQuerier: tx,
				commit:     tx.Commit,
				rollback:   func() { _ = tx.Rollback() },
			}, nil
		},
		bind:        bindNumbered,
		uniqueIndex: postgresSharedVolumeUniqueIndex,
	}
}

func postgresSharedVolumeUniqueIndex(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName
	}
	return ""
}

func (s *PostgresStore) ReserveSharedVolume(ctx context.Context, volume *SharedVolume, maxLive int) (*SharedVolume, error) {
	return s.sharedVolumes().reserve(ctx, volume, maxLive)
}

func (s *PostgresStore) RenewSharedVolumeLease(ctx context.Context, id string, leaseUntil int64) (bool, error) {
	return s.sharedVolumes().renew(ctx, id, leaseUntil)
}

func (s *PostgresStore) FinishSharedVolume(ctx context.Context, volume *SharedVolume) (bool, error) {
	return s.sharedVolumes().finish(ctx, volume)
}

func (s *PostgresStore) FailSharedVolume(ctx context.Context, id string, now int64) (bool, error) {
	return s.sharedVolumes().fail(ctx, id, now)
}

func (s *PostgresStore) NameSharedVolume(ctx context.Context, id, name string) (bool, error) {
	return s.sharedVolumes().name(ctx, id, name)
}

func (s *PostgresStore) GetSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().get(ctx, sha256)
}

func (s *PostgresStore) ListSharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error) {
	return s.sharedVolumes().list(ctx, name)
}

func (s *PostgresStore) DeleteSharedVolume(ctx context.Context, sha256 string, now int64) (*SharedVolume, error) {
	return s.sharedVolumes().delete(ctx, sha256, now)
}

func (s *PostgresStore) RemoveSharedVolume(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().remove(ctx, id)
}
