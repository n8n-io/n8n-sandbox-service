package store

import (
	"context"
	"errors"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLite ignores VARCHAR lengths, so CHECKs hold the same limits as Postgres.
const sqliteSharedVolumesSchema = `
CREATE TABLE IF NOT EXISTS shared_volumes (
	id                  TEXT    PRIMARY KEY CHECK (length(id) <= 64),
	sha256              TEXT    NOT NULL CHECK (length(sha256) = 64),
	name                TEXT    NOT NULL DEFAULT '' CHECK (length(name) <= 128),
	state               TEXT    NOT NULL CHECK (state IN ('publishing', 'ready', 'failed', 'deleted')),
	tar_bytes           INTEGER NOT NULL DEFAULT 0,
	entries             INTEGER NOT NULL DEFAULT 0,
	content_bytes       INTEGER NOT NULL DEFAULT 0,
	decompressed_bytes  INTEGER NOT NULL DEFAULT 0,
	image_bytes         INTEGER NOT NULL DEFAULT 0,
	image_sha256        TEXT    NOT NULL DEFAULT '' CHECK (length(image_sha256) <= 64),
	published_by        TEXT    NOT NULL CHECK (published_by IN ('admin', 'publisher')),
	created_at          INTEGER NOT NULL,
	ready_at            INTEGER NOT NULL DEFAULT 0,
	updated_at          INTEGER NOT NULL,
	lease_until         INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesSHA256Index + ` ON shared_volumes (sha256) WHERE ` + sharedVolumesLive + `;
CREATE UNIQUE INDEX IF NOT EXISTS ` + sharedVolumesNameIndex + ` ON shared_volumes (name) WHERE name <> '' AND ` + sharedVolumesLive + `;
CREATE INDEX IF NOT EXISTS shared_volumes_state_idx ON shared_volumes (state, updated_at);
`

func (s *SQLiteStore) sharedVolumes() sharedVolumeSQL {
	return sharedVolumeSQL{
		db: s.db,
		begin: func(context.Context) (*sqlTx, error) {
			conn, err := s.beginImmediate()
			if err != nil {
				return nil, err
			}
			return &sqlTx{
				sqlQuerier: conn,
				commit:     func() error { return commitConn(conn) },
				rollback:   func() { rollbackConn(conn) },
			}, nil
		},
		bind:        func(query string) string { return query },
		uniqueIndex: sqliteSharedVolumeUniqueIndex,
	}
}

// sqliteSharedVolumeUniqueIndex names the index of a unique violation. SQLite
// names the columns, not the index, of a partial index.
func sqliteSharedVolumeUniqueIndex(err error) string {
	var serr *sqlite.Error
	if !errors.As(err, &serr) || serr.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return ""
	}
	switch {
	case strings.Contains(serr.Error(), "shared_volumes.sha256"):
		return sharedVolumesSHA256Index
	case strings.Contains(serr.Error(), "shared_volumes.name"):
		return sharedVolumesNameIndex
	}
	return ""
}

func (s *SQLiteStore) ReserveSharedVolume(ctx context.Context, volume *SharedVolume, maxLive int) (*SharedVolume, error) {
	return s.sharedVolumes().reserve(ctx, volume, maxLive)
}

func (s *SQLiteStore) RenewSharedVolumeLease(ctx context.Context, id string, leaseUntil int64) (bool, error) {
	return s.sharedVolumes().renew(ctx, id, leaseUntil)
}

func (s *SQLiteStore) FinishSharedVolume(ctx context.Context, volume *SharedVolume) (bool, error) {
	return s.sharedVolumes().finish(ctx, volume)
}

func (s *SQLiteStore) FailSharedVolume(ctx context.Context, id string, now int64) (bool, error) {
	return s.sharedVolumes().fail(ctx, id, now)
}

func (s *SQLiteStore) NameSharedVolume(ctx context.Context, id, name string) (bool, error) {
	return s.sharedVolumes().name(ctx, id, name)
}

func (s *SQLiteStore) GetSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return s.sharedVolumes().get(ctx, sha256)
}

func (s *SQLiteStore) ListSharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error) {
	return s.sharedVolumes().list(ctx, name)
}

func (s *SQLiteStore) DeleteSharedVolume(ctx context.Context, sha256 string, now int64) (*SharedVolume, error) {
	return s.sharedVolumes().delete(ctx, sha256, now)
}

func (s *SQLiteStore) RemoveSharedVolume(ctx context.Context, id string) (bool, error) {
	return s.sharedVolumes().remove(ctx, id)
}
