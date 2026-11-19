package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SharedVolumeState is where a shared volume is in its life.
type SharedVolumeState string

const (
	// SharedVolumePublishing is a row whose publish holds its lease and may be
	// storing its objects.
	SharedVolumePublishing SharedVolumeState = "publishing"
	SharedVolumeReady      SharedVolumeState = "ready"
	// SharedVolumeFailed is a publish that did not finish. Its objects may
	// exist until the row is removed.
	SharedVolumeFailed SharedVolumeState = "failed"
	// SharedVolumeDeleted is a ready volume a caller deleted: hidden, and
	// removed with its objects.
	SharedVolumeDeleted SharedVolumeState = "deleted"
)

// SharedVolume is a row of shared_volumes: one published tar, by its sha256.
// Times are Unix seconds.
type SharedVolume struct {
	// ID is chosen by the API and names the volume's objects in the volume
	// store, so no two rows share an object.
	ID     string
	SHA256 string
	// Name is "" for none. Publishing and ready rows have different names.
	Name              string
	State             SharedVolumeState
	TarBytes          int64
	Entries           int64
	ContentBytes      int64
	DecompressedBytes int64
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
)

// SharedVolumes keeps the shared_volumes rows. Rows are written before their
// objects and removed after them, so every object has a row. A publishing row
// whose lease ended before now is taken over: marked failed.
type SharedVolumes interface {
	// ReserveSharedVolume inserts volume, which must be publishing, with
	// volume.UpdatedAt as now. It first marks failed the publishing rows with
	// its sha256 or name whose lease has ended. It returns
	// ErrSharedVolumeDigestLive, with that row when it could read it, when a
	// publishing or ready row has the sha256; ErrSharedVolumeNameTaken when
	// one has the name; ErrSharedVolumeLimit when maxLive rows are publishing
	// or ready. On Postgres, concurrent reservations can pass maxLive by their
	// number.
	ReserveSharedVolume(ctx context.Context, volume *SharedVolume, maxLive int) (*SharedVolume, error)
	// RenewSharedVolumeLease moves a publishing row's lease to leaseUntil. It
	// returns false when the row is no longer publishing.
	RenewSharedVolumeLease(ctx context.Context, id string, leaseUntil int64) (bool, error)
	// FinishSharedVolume makes the publishing row volume.ID ready with
	// volume's sizes, image and ReadyAt. It returns false when the row is no
	// longer publishing.
	FinishSharedVolume(ctx context.Context, volume *SharedVolume) (bool, error)
	// FailSharedVolume marks a publishing row failed. It returns whether the
	// row is failed now, so false for a ready row, whose objects stay.
	FailSharedVolume(ctx context.Context, id string, now int64) (bool, error)
	// NameSharedVolume names a ready row that has no name. It returns false
	// when there is no such row, and ErrSharedVolumeNameTaken.
	NameSharedVolume(ctx context.Context, id, name string) (bool, error)
	// GetSharedVolume returns the publishing or ready row with the sha256, or
	// nil.
	GetSharedVolume(ctx context.Context, sha256 string) (*SharedVolume, error)
	// ListSharedVolumes returns the ready rows, newest first; only the one
	// with the name when name is set.
	ListSharedVolumes(ctx context.Context, name string) ([]*SharedVolume, error)
	// DeleteSharedVolume marks the ready row with the sha256 deleted, after
	// marking failed a publishing row with it whose lease has ended. It returns
	// the row with the sha256 that is still publishing or now deleted, or nil.
	DeleteSharedVolume(ctx context.Context, sha256 string, now int64) (*SharedVolume, error)
	// RemoveSharedVolume deletes the row unless it is ready. It returns false
	// when there was no such row.
	RemoveSharedVolume(ctx context.Context, id string) (bool, error)
}

var (
	_ SharedVolumes = (*SQLiteStore)(nil)
	_ SharedVolumes = (*PostgresStore)(nil)
)

// Index names, which Postgres reports on a unique violation.
const (
	sharedVolumesSHA256Index = "shared_volumes_live_sha256_uniq"
	sharedVolumesNameIndex   = "shared_volumes_live_name_uniq"
)

// sharedVolumesLive selects the rows that hold their sha256 and name.
const sharedVolumesLive = `state IN ('publishing', 'ready')`

const sharedVolumeCols = `id, sha256, name, state, tar_bytes, entries, content_bytes, decompressed_bytes, image_bytes, image_sha256, published_by, created_at, ready_at, updated_at, lease_until`

// sqlQuerier is what *sql.DB, *sql.Conn and *sql.Tx share.
type sqlQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqlTx is a transaction that writes; SQLite runs one at a time.
type sqlTx struct {
	sqlQuerier
	commit   func() error
	rollback func()
}

// sharedVolumeSQL runs the shared_volumes queries, written with ? placeholders,
// on either database.
type sharedVolumeSQL struct {
	db    *sql.DB
	begin func(ctx context.Context) (*sqlTx, error)
	// bind rewrites the ? placeholders of a query for the database.
	bind func(query string) string
	// uniqueIndex returns the index a unique violation is on, or "".
	uniqueIndex func(err error) string
}

func scanSharedVolume(row scanner) (*SharedVolume, error) {
	var volume SharedVolume
	var state string
	err := row.Scan(&volume.ID, &volume.SHA256, &volume.Name, &state, &volume.TarBytes, &volume.Entries,
		&volume.ContentBytes, &volume.DecompressedBytes, &volume.ImageBytes, &volume.ImageSHA256,
		&volume.PublishedBy, &volume.CreatedAt, &volume.ReadyAt, &volume.UpdatedAt, &volume.LeaseUntil)
	if err != nil {
		return nil, err
	}
	volume.State = SharedVolumeState(state)
	return &volume, nil
}

// getOne returns the row the query selects, or nil.
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

// changed runs an update or delete and reports whether it changed a row.
func (q sharedVolumeSQL) changed(ctx context.Context, querier sqlQuerier, query string, args ...any) (bool, error) {
	result, err := querier.ExecContext(ctx, q.bind(query), args...)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// failLapsed marks failed the publishing rows whose lease ended before now and
// that have the sha256 or, if set, the name.
func (q sharedVolumeSQL) failLapsed(ctx context.Context, tx *sqlTx, sha256, name string, now int64) error {
	query := `UPDATE shared_volumes SET state = 'failed', updated_at = ?, lease_until = 0
		WHERE state = 'publishing' AND lease_until < ? AND (sha256 = ?`
	args := []any{now, now, sha256}
	if name != "" {
		query += ` OR name = ?`
		args = append(args, name)
	}
	if _, err := tx.ExecContext(ctx, q.bind(query+`)`), args...); err != nil {
		return fmt.Errorf("store: take over shared volume leases: %w", err)
	}
	return nil
}

// inTx runs work in a transaction and commits it unless work returns an error
// that wantsRollback reports. Other errors from work are returned after the
// commit, so a takeover done before them is kept.
func (q sharedVolumeSQL) inTx(ctx context.Context, work func(tx *sqlTx) error, wantsRollback func(error) bool) error {
	tx, err := q.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin shared volume transaction: %w", err)
	}
	workErr := work(tx)
	if workErr != nil && wantsRollback(workErr) {
		tx.rollback()
		return workErr
	}
	if err := tx.commit(); err != nil {
		return fmt.Errorf("store: commit shared volume transaction: %w", err)
	}
	return workErr
}

// sqlError marks an error that rolls a transaction back.
type sqlError struct{ err error }

func (e sqlError) Error() string { return e.err.Error() }
func (e sqlError) Unwrap() error { return e.err }

func isSQLError(err error) bool {
	var target sqlError
	return errors.As(err, &target)
}

func (q sharedVolumeSQL) reserve(ctx context.Context, volume *SharedVolume, maxLive int) (*SharedVolume, error) {
	if volume.State != SharedVolumePublishing {
		return nil, fmt.Errorf("store: reserve a shared volume in state %q", volume.State)
	}
	var existing *SharedVolume
	err := q.inTx(ctx, func(tx *sqlTx) error {
		if err := q.failLapsed(ctx, tx, volume.SHA256, volume.Name, volume.UpdatedAt); err != nil {
			return sqlError{err}
		}
		live, err := q.getOne(ctx, tx, `SELECT `+sharedVolumeCols+` FROM shared_volumes WHERE sha256 = ? AND `+sharedVolumesLive, volume.SHA256)
		if err != nil {
			return sqlError{err}
		}
		if live != nil {
			existing = live
			return ErrSharedVolumeDigestLive
		}
		if volume.Name != "" {
			var taken int
			err := tx.QueryRowContext(ctx, q.bind(`SELECT COUNT(*) FROM shared_volumes WHERE name = ? AND `+sharedVolumesLive), volume.Name).Scan(&taken)
			if err != nil {
				return sqlError{fmt.Errorf("store: check shared volume name: %w", err)}
			}
			if taken > 0 {
				return ErrSharedVolumeNameTaken
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shared_volumes WHERE `+sharedVolumesLive).Scan(&count); err != nil {
			return sqlError{fmt.Errorf("store: count shared volumes: %w", err)}
		}
		if count >= maxLive {
			return ErrSharedVolumeLimit
		}
		_, err = tx.ExecContext(ctx, q.bind(`INSERT INTO shared_volumes (`+sharedVolumeCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			volume.ID, volume.SHA256, volume.Name, string(volume.State), volume.TarBytes, volume.Entries,
			volume.ContentBytes, volume.DecompressedBytes, volume.ImageBytes, volume.ImageSHA256,
			volume.PublishedBy, volume.CreatedAt, volume.ReadyAt, volume.UpdatedAt, volume.LeaseUntil)
		switch q.uniqueIndex(err) {
		case sharedVolumesSHA256Index:
			return sqlError{ErrSharedVolumeDigestLive}
		case sharedVolumesNameIndex:
			return sqlError{ErrSharedVolumeNameTaken}
		}
		if err != nil {
			return sqlError{fmt.Errorf("store: insert shared volume: %w", err)}
		}
		return nil
	}, isSQLError)
	if errors.Is(err, ErrSharedVolumeDigestLive) && existing == nil {
		// Another pod inserted it after this one looked.
		existing, _ = q.get(ctx, volume.SHA256)
	}
	return existing, unwrapSQLError(err)
}

func unwrapSQLError(err error) error {
	var target sqlError
	if errors.As(err, &target) && (errors.Is(target.err, ErrSharedVolumeDigestLive) || errors.Is(target.err, ErrSharedVolumeNameTaken)) {
		return target.err
	}
	return err
}

func (q sharedVolumeSQL) renew(ctx context.Context, id string, leaseUntil int64) (bool, error) {
	ok, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET lease_until = ? WHERE id = ? AND state = 'publishing'`, leaseUntil, id)
	if err != nil {
		return false, fmt.Errorf("store: renew shared volume lease: %w", err)
	}
	return ok, nil
}

func (q sharedVolumeSQL) finish(ctx context.Context, volume *SharedVolume) (bool, error) {
	ok, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET state = 'ready', tar_bytes = ?, entries = ?,
		content_bytes = ?, decompressed_bytes = ?, image_bytes = ?, image_sha256 = ?, ready_at = ?,
		updated_at = ?, lease_until = 0
		WHERE id = ? AND state = 'publishing'`,
		volume.TarBytes, volume.Entries, volume.ContentBytes, volume.DecompressedBytes, volume.ImageBytes,
		volume.ImageSHA256, volume.ReadyAt, volume.ReadyAt, volume.ID)
	if err != nil {
		return false, fmt.Errorf("store: finish shared volume: %w", err)
	}
	return ok, nil
}

func (q sharedVolumeSQL) fail(ctx context.Context, id string, now int64) (bool, error) {
	if _, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET state = 'failed', updated_at = ?, lease_until = 0
		WHERE id = ? AND state = 'publishing'`, now, id); err != nil {
		return false, fmt.Errorf("store: fail shared volume: %w", err)
	}
	var failed int
	if err := q.db.QueryRowContext(ctx, q.bind(`SELECT COUNT(*) FROM shared_volumes WHERE id = ? AND state = 'failed'`), id).Scan(&failed); err != nil {
		return false, fmt.Errorf("store: fail shared volume: %w", err)
	}
	return failed > 0, nil
}

func (q sharedVolumeSQL) name(ctx context.Context, id, name string) (bool, error) {
	ok, err := q.changed(ctx, q.db, `UPDATE shared_volumes SET name = ? WHERE id = ? AND state = 'ready' AND name = ''`, name, id)
	if q.uniqueIndex(err) == sharedVolumesNameIndex {
		return false, ErrSharedVolumeNameTaken
	}
	if err != nil {
		return false, fmt.Errorf("store: name shared volume: %w", err)
	}
	return ok, nil
}

func (q sharedVolumeSQL) get(ctx context.Context, sha256 string) (*SharedVolume, error) {
	return q.getOne(ctx, q.db, `SELECT `+sharedVolumeCols+` FROM shared_volumes WHERE sha256 = ? AND `+sharedVolumesLive, sha256)
}

func (q sharedVolumeSQL) list(ctx context.Context, name string) ([]*SharedVolume, error) {
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

func (q sharedVolumeSQL) delete(ctx context.Context, sha256 string, now int64) (*SharedVolume, error) {
	var found *SharedVolume
	err := q.inTx(ctx, func(tx *sqlTx) error {
		if err := q.failLapsed(ctx, tx, sha256, "", now); err != nil {
			return err
		}
		live, err := q.getOne(ctx, tx, `SELECT `+sharedVolumeCols+` FROM shared_volumes WHERE sha256 = ? AND `+sharedVolumesLive, sha256)
		if err != nil || live == nil || live.State != SharedVolumeReady {
			found = live
			return err
		}
		if _, err := tx.ExecContext(ctx, q.bind(`UPDATE shared_volumes SET state = 'deleted', updated_at = ? WHERE id = ? AND state = 'ready'`), now, live.ID); err != nil {
			return fmt.Errorf("store: delete shared volume: %w", err)
		}
		live.State, live.UpdatedAt = SharedVolumeDeleted, now
		found = live
		return nil
	}, func(error) bool { return true })
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (q sharedVolumeSQL) remove(ctx context.Context, id string) (bool, error) {
	ok, err := q.changed(ctx, q.db, `DELETE FROM shared_volumes WHERE id = ? AND state <> 'ready'`, id)
	if err != nil {
		return false, fmt.Errorf("store: remove shared volume: %w", err)
	}
	return ok, nil
}

// bindNumbered rewrites ? placeholders as $1, $2, … for Postgres. The queries
// here have no ? anywhere else.
func bindNumbered(query string) string {
	var out strings.Builder
	number := 0
	for _, char := range query {
		if char == '?' {
			number++
			fmt.Fprintf(&out, "$%d", number)
			continue
		}
		out.WriteRune(char)
	}
	return out.String()
}
