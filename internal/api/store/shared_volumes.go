// This file defines the shared_volumes rows and the queries on them, for SQLite
// and Postgres.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SharedVolumeState is where a shared volume is in its life.
type SharedVolumeState string

const (
	// SharedVolumePublishing is a row whose publish holds its lease and may be
	// storing its objects.
	SharedVolumePublishing SharedVolumeState = "publishing"
	SharedVolumeReady      SharedVolumeState = "ready"
	// SharedVolumeFailed is a publish that did not finish. Its objects may
	// exist until the row is purged.
	SharedVolumeFailed SharedVolumeState = "failed"
	// SharedVolumeDeleted is a ready volume a caller deleted: hidden, and
	// purged with its objects.
	SharedVolumeDeleted SharedVolumeState = "deleted"
)

// SharedVolume is a row of shared_volumes: one published tar, by its sha256.
// Times are Unix seconds by the database's clock.
type SharedVolume struct {
	// ID is chosen by the API and names the volume's objects in the volume
	// store, so no two rows share an object.
	ID     string
	SHA256 string
	// Name is "" for none. Publishing and ready rows have different names.
	Name  string
	State SharedVolumeState
	// TarBytes is the tar's length as published, plain or gzip.
	TarBytes             int64
	TarEntries           int64
	TarContentBytes      int64
	TarDecompressedBytes int64
	// ImageBytes is 0 for a volume without an ext4 image.
	ImageBytes  int64
	ImageSHA256 string
	// PublishedBy is the role of the key that published it.
	PublishedBy string
	CreatedAt   int64
	ReadyAt     int64
	// UpdatedAt is when the state last changed.
	UpdatedAt int64
	// LeaseUntil is when a publishing row's lease ends.
	LeaseUntil int64
}

var (
	// ErrSharedVolumeDigestLive is returned when a publishing or ready row has
	// the sha256.
	ErrSharedVolumeDigestLive = errors.New("a shared volume with this sha256 exists")
	// ErrSharedVolumeNameTaken is returned when a publishing or ready row has
	// the name.
	ErrSharedVolumeNameTaken = errors.New("shared volume name taken")
	// ErrSharedVolumeLimit is returned when the publishing and ready rows are
	// already at the limit.
	ErrSharedVolumeLimit = errors.New("too many shared volumes")
	// ErrSharedVolumeNameEmpty is returned when a row is to get an empty name.
	ErrSharedVolumeNameEmpty = errors.New("shared volume name is empty")
)

// SharedVolumes keeps the shared_volumes rows. Rows are written before their
// objects and purged after them, so every object has a row. A publishing row
// whose lease has ended is taken over: marked failed.
type SharedVolumes interface {
	// StartSharedVolumePublish inserts volume as a publishing row whose lease
	// ends lease from now, and sets volume's state and times. It first fails
	// the publishing rows with its sha256 or name whose lease has ended. It
	// returns ErrSharedVolumeDigestLive, with the live row when there still is
	// one, when a publishing or ready row has the sha256;
	// ErrSharedVolumeNameTaken when one has the name; ErrSharedVolumeLimit
	// when it would make more than maxLiveVolumes rows publishing or ready.
	StartSharedVolumePublish(ctx context.Context, volume *SharedVolume, lease time.Duration, maxLiveVolumes int) (*SharedVolume, error)
	// RenewSharedVolumePublish moves the lease of a publishing row to end
	// lease from now. It returns false when the row is no longer publishing.
	RenewSharedVolumePublish(ctx context.Context, id string, lease time.Duration) (bool, error)
	// FinishSharedVolumePublish makes the publishing row volume.ID ready with
	// volume's sizes and image, and sets volume's state and times. It returns
	// false when the row is no longer publishing.
	FinishSharedVolumePublish(ctx context.Context, volume *SharedVolume) (bool, error)
	// FailSharedVolumePublish marks a publishing row failed. It returns
	// whether the row is failed now, so true for a row another publish took
	// over, and false for a ready or deleted row, whose objects stay.
	FailSharedVolumePublish(ctx context.Context, id string) (bool, error)
	// NameReadySharedVolume names the ready row id if it has no name. It
	// returns the name the row has afterwards, which differs from name when
	// the row was named before with another name, and "" when there is no
	// such ready row. It returns ErrSharedVolumeNameEmpty for an empty name,
	// and ErrSharedVolumeNameTaken when another live row has the name.
	NameReadySharedVolume(ctx context.Context, id, name string) (string, error)
	// GetLiveSharedVolume returns the ready row with the sha256, or the
	// publishing one whose lease has not ended, or nil.
	GetLiveSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error)
	// ListReadySharedVolumes returns the ready rows, newest first; only the
	// one with the name when name is set.
	ListReadySharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error)
	// MarkSharedVolumeDeleted marks the ready row with the sha256 deleted,
	// after failing a publishing row with it whose lease has ended. It returns
	// the deleted row, else the row still publishing the sha256, else nil.
	MarkSharedVolumeDeleted(ctx context.Context, sha256 string) (*SharedVolume, error)
	// PurgeSharedVolume deletes the row id if it is failed or deleted. It
	// returns false when there is no such row.
	PurgeSharedVolume(ctx context.Context, id string) (bool, error)
	// FailLapsedSharedVolumePublishes marks failed every publishing row whose
	// lease has run out, and returns how many it marked.
	FailLapsedSharedVolumePublishes(ctx context.Context) (int64, error)
	// ListPurgeableSharedVolumes returns the failed and deleted rows whose
	// state changed at least minAge ago. It returns up to limit rows after
	// the position after, the oldest change first.
	ListPurgeableSharedVolumes(ctx context.Context, minAge time.Duration, after SharedVolumePosition, limit int) ([]*SharedVolume, error)
}

// SharedVolumePosition is a row's place in ListPurgeableSharedVolumes's
// order. The zero value is before every row.
type SharedVolumePosition struct {
	UpdatedAt int64
	ID        string
}

var (
	_ SharedVolumes = (*SQLiteStore)(nil)
	_ SharedVolumes = (*PostgresStore)(nil)
)

// Unique index names, which Postgres reports on a violation. SQLite reports
// the column instead.
const (
	sharedVolumesSHA256Index = "shared_volumes_live_sha256_uniq"
	sharedVolumesNameIndex   = "shared_volumes_live_name_uniq"
)

// sharedVolumesLive selects the rows that hold their sha256 and name.
const sharedVolumesLive = `state IN ('publishing', 'ready')`

// sharedVolumesLiveCountLock is the lock name of the transactions that insert
// a row and then count the live rows against a limit. The lock makes them wait
// for each other, so the count is exact.
const sharedVolumesLiveCountLock = "shared_volumes:live_count"

const sharedVolumeCols = `id, sha256, name, state, tar_bytes, tar_entries, tar_content_bytes, tar_decompressed_bytes, image_bytes, image_sha256, published_by, created_at, ready_at, updated_at, lease_until`

// sharedVolumeSQL runs the shared_volumes queries.
type sharedVolumeSQL struct{ sqlDialect }

func scanSharedVolume(row scanner) (*SharedVolume, error) {
	var volume SharedVolume
	var state string
	err := row.Scan(&volume.ID, &volume.SHA256, &volume.Name, &state, &volume.TarBytes, &volume.TarEntries,
		&volume.TarContentBytes, &volume.TarDecompressedBytes, &volume.ImageBytes, &volume.ImageSHA256,
		&volume.PublishedBy, &volume.CreatedAt, &volume.ReadyAt, &volume.UpdatedAt, &volume.LeaseUntil)
	if err != nil {
		return nil, err
	}
	volume.State = SharedVolumeState(state)
	return &volume, nil
}

// getOne returns the row the query selects or returns, or nil.
func (q sharedVolumeSQL) getOne(ctx context.Context, querier sqlQuerier, query string, args ...any) (*SharedVolume, error) {
	volume, err := scanSharedVolume(querier.QueryRowContext(ctx, q.bind(query), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get shared volume: %w", err)
	}
	return volume, nil
}

// uniqueViolation returns the error a unique violation in err stands for, or
// nil.
func (q sharedVolumeSQL) uniqueViolation(err error) error {
	switch {
	case q.violatesUnique(err, sharedVolumesSHA256Index, "shared_volumes.sha256"):
		return ErrSharedVolumeDigestLive
	case q.violatesUnique(err, sharedVolumesNameIndex, "shared_volumes.name"):
		return ErrSharedVolumeNameTaken
	}
	return nil
}

// failLapsedPublishes marks failed the publishing rows whose lease has ended
// and that have the sha256 or, if set, the name.
func (q sharedVolumeSQL) failLapsedPublishes(ctx context.Context, sha256, name string) error {
	query := `UPDATE shared_volumes SET state = 'failed', updated_at = ` + q.nowSQL + `, lease_until = 0
		WHERE state = 'publishing' AND lease_until < ` + q.nowSQL + ` AND (sha256 = ?`
	args := []any{sha256}
	if name != "" {
		query += ` OR name = ?`
		args = append(args, name)
	}
	if _, err := q.db.ExecContext(ctx, q.bind(query+`)`), args...); err != nil {
		return fmt.Errorf("store: take over lapsed shared volume publishes: %w", err)
	}
	return nil
}

func leaseSeconds(lease time.Duration) int64 { return int64(lease / time.Second) }

func (q sharedVolumeSQL) startPublish(ctx context.Context, volume *SharedVolume, lease time.Duration, maxLiveVolumes int) (*SharedVolume, error) {
	if err := q.failLapsedPublishes(ctx, volume.SHA256, volume.Name); err != nil {
		return nil, err
	}
	err := q.inTx(ctx, func(tx *sqlTx) error {
		if err := q.lock(ctx, tx, sharedVolumesLiveCountLock); err != nil {
			return err
		}
		inserted := tx.QueryRowContext(ctx, q.bind(`INSERT INTO shared_volumes
			(id, sha256, name, state, published_by, created_at, updated_at, lease_until)
			VALUES (?, ?, ?, 'publishing', ?, `+q.nowSQL+`, `+q.nowSQL+`, `+q.nowSQL+` + ?)
			RETURNING created_at, lease_until`),
			volume.ID, volume.SHA256, volume.Name, volume.PublishedBy, leaseSeconds(lease))
		if err := inserted.Scan(&volume.CreatedAt, &volume.LeaseUntil); err != nil {
			if violation := q.uniqueViolation(err); violation != nil {
				return violation
			}
			return fmt.Errorf("store: insert shared volume: %w", err)
		}
		var liveVolumes int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shared_volumes WHERE `+sharedVolumesLive).Scan(&liveVolumes); err != nil {
			return fmt.Errorf("store: count shared volumes: %w", err)
		}
		if liveVolumes > maxLiveVolumes {
			return ErrSharedVolumeLimit
		}
		return nil
	})
	switch {
	case errors.Is(err, ErrSharedVolumeDigestLive), errors.Is(err, ErrSharedVolumeNameTaken):
		// When both indexes refuse the row, the database picks which one it
		// reports: a live row with the sha256 decides.
		existing, getErr := q.getLive(ctx, volume.SHA256)
		if getErr != nil {
			return nil, getErr
		}
		if existing != nil {
			return existing, ErrSharedVolumeDigestLive
		}
		return nil, err
	case err != nil:
		return nil, err
	}
	volume.State, volume.UpdatedAt = SharedVolumePublishing, volume.CreatedAt
	return nil, nil
}

func (q sharedVolumeSQL) renewPublish(ctx context.Context, id string, lease time.Duration) (bool, error) {
	renewed, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET lease_until = `+q.nowSQL+` + ?
		WHERE id = ? AND state = 'publishing'`, leaseSeconds(lease), id)
	if err != nil {
		return false, fmt.Errorf("store: renew shared volume publish: %w", err)
	}
	return renewed, nil
}

func (q sharedVolumeSQL) finishPublish(ctx context.Context, volume *SharedVolume) (bool, error) {
	err := q.db.QueryRowContext(ctx, q.bind(`UPDATE shared_volumes SET state = 'ready', tar_bytes = ?,
		tar_entries = ?, tar_content_bytes = ?, tar_decompressed_bytes = ?, image_bytes = ?, image_sha256 = ?,
		ready_at = `+q.nowSQL+`, updated_at = `+q.nowSQL+`, lease_until = 0
		WHERE id = ? AND state = 'publishing'
		RETURNING ready_at`),
		volume.TarBytes, volume.TarEntries, volume.TarContentBytes, volume.TarDecompressedBytes,
		volume.ImageBytes, volume.ImageSHA256, volume.ID).Scan(&volume.ReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: finish shared volume publish: %w", err)
	}
	volume.State, volume.UpdatedAt, volume.LeaseUntil = SharedVolumeReady, volume.ReadyAt, 0
	return true, nil
}

func (q sharedVolumeSQL) failPublish(ctx context.Context, id string) (bool, error) {
	failed, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET state = 'failed', lease_until = 0,
		updated_at = CASE WHEN state = 'failed' THEN updated_at ELSE `+q.nowSQL+` END
		WHERE id = ? AND state IN ('publishing', 'failed')`, id)
	if err != nil {
		return false, fmt.Errorf("store: fail shared volume publish: %w", err)
	}
	return failed, nil
}

// setName takes the row in either naming state and changes only a name that
// is empty, so the caller learns the name the row has without a second read.
func (q sharedVolumeSQL) setName(ctx context.Context, id, name string) (string, error) {
	if name == "" {
		return "", ErrSharedVolumeNameEmpty
	}
	var storedName string
	err := q.db.QueryRowContext(ctx, q.bind(`UPDATE shared_volumes
		SET name = CASE WHEN name = '' THEN ? ELSE name END
		WHERE id = ? AND state = 'ready'
		RETURNING name`), name, id).Scan(&storedName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if violation := q.uniqueViolation(err); violation != nil {
		return "", violation
	}
	if err != nil {
		return "", fmt.Errorf("store: name shared volume: %w", err)
	}
	return storedName, nil
}

func (q sharedVolumeSQL) getLive(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return q.getOne(ctx, q.db, `SELECT `+sharedVolumeCols+` FROM shared_volumes WHERE sha256 = ?
		AND (state = 'ready' OR (state = 'publishing' AND lease_until >= `+q.nowSQL+`))`, sha256)
}

func (q sharedVolumeSQL) listReady(ctx context.Context, name string) ([]*SharedVolume, error) {
	query := `SELECT ` + sharedVolumeCols + ` FROM shared_volumes WHERE state = 'ready'`
	var args []any
	if name != "" {
		query += ` AND name = ?`
		args = append(args, name)
	}
	rows, err := q.db.QueryContext(ctx, q.bind(query+` ORDER BY ready_at DESC, id`), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list shared volumes: %w", err)
	}
	defer rows.Close()
	var volumes []*SharedVolume
	for rows.Next() {
		volume, err := scanSharedVolume(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list shared volumes: %w", err)
		}
		volumes = append(volumes, volume)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list shared volumes: %w", err)
	}
	return volumes, nil
}

func (q sharedVolumeSQL) markDeleted(ctx context.Context, sha256 string) (*SharedVolume, error) {
	if err := q.failLapsedPublishes(ctx, sha256, ""); err != nil {
		return nil, err
	}
	// One statement takes the live row in either state and changes it only if
	// it is ready, so a publish that finishes at the same time can't leave the
	// delete with neither.
	return q.getOne(ctx, q.db, `UPDATE shared_volumes
		SET state = CASE WHEN state = 'ready' THEN 'deleted' ELSE state END,
			updated_at = CASE WHEN state = 'ready' THEN `+q.nowSQL+` ELSE updated_at END
		WHERE sha256 = ? AND `+sharedVolumesLive+`
		RETURNING `+sharedVolumeCols, sha256)
}

func (q sharedVolumeSQL) purge(ctx context.Context, id string) (bool, error) {
	purged, err := q.changed(ctx, q.db, `DELETE FROM shared_volumes WHERE id = ? AND state IN ('failed', 'deleted')`, id)
	if err != nil {
		return false, fmt.Errorf("store: purge shared volume: %w", err)
	}
	return purged, nil
}
