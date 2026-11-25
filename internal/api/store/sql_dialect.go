package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

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

// sqlDialect runs queries written with ? placeholders on SQLite or Postgres.
type sqlDialect struct {
	db    *sql.DB
	begin func(ctx context.Context) (*sqlTx, error)
	// bind rewrites the ? placeholders of a query for the database.
	bind func(query string) string
	// nowSQL is the database clock's Unix time in whole seconds, as of the start
	// of the statement. Not of the transaction: a statement that comes after a
	// wait for a lock must not get a time from before the wait.
	nowSQL string
	// lockSQL, if set, takes a lock on the name it gets as its one ? and holds
	// it until the transaction ends. SQLite has none: its write transactions
	// already run one at a time.
	lockSQL string
	// violatesUnique reports whether err is a violation of the unique index
	// named index, on the column table.column. SQLite names the column, not
	// the index, of a partial index.
	violatesUnique func(err error, index, column string) bool
}

func newSQLiteDialect(store *SQLiteStore) sqlDialect {
	return sqlDialect{
		db: store.db,
		begin: func(ctx context.Context) (*sqlTx, error) {
			conn, err := store.beginImmediateContext(ctx)
			if err != nil {
				return nil, err
			}
			return &sqlTx{
				sqlQuerier: conn,
				commit:     func() error { return commitConn(conn) },
				rollback:   func() { rollbackConn(conn) },
			}, nil
		},
		bind:   func(query string) string { return query },
		nowSQL: `unixepoch()`,
		violatesUnique: func(err error, _, column string) bool {
			var sqliteErr *sqlite.Error
			return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE &&
				strings.Contains(sqliteErr.Error(), column)
		},
	}
}

func newPostgresDialect(store *PostgresStore) sqlDialect {
	return sqlDialect{
		db: store.db,
		begin: func(ctx context.Context) (*sqlTx, error) {
			// Not the server default: after lock returns, a statement must see
			// what the last holder of the lock committed. Only READ COMMITTED
			// takes its snapshot per statement, not before the lock.
			tx, err := store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				return nil, err
			}
			return &sqlTx{
				sqlQuerier: tx,
				commit:     tx.Commit,
				rollback:   func() { _ = tx.Rollback() },
			}, nil
		},
		bind:    bindNumbered,
		nowSQL:  `CAST(FLOOR(EXTRACT(EPOCH FROM statement_timestamp())) AS BIGINT)`,
		lockSQL: `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,
		violatesUnique: func(err error, index, _ string) bool {
			var pgErr *pgconn.PgError
			return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == index
		},
	}
}

// changed runs an update or delete and reports whether it changed a row.
func (d sqlDialect) changed(ctx context.Context, querier sqlQuerier, query string, args ...any) (bool, error) {
	result, err := querier.ExecContext(ctx, d.bind(query), args...)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// inTx runs work in a transaction, and commits it unless work returns an
// error.
func (d sqlDialect) inTx(ctx context.Context, work func(tx *sqlTx) error) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	if err := work(tx); err != nil {
		tx.rollback()
		return err
	}
	if err := tx.commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// lock makes tx wait until every other transaction that locked the same name
// has ended, and keeps others waiting until tx ends. Names are shared by all
// features of the store, so start one with the feature, like "sandbox:".
func (d sqlDialect) lock(ctx context.Context, tx *sqlTx, name string) error {
	if d.lockSQL == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, d.bind(d.lockSQL), name); err != nil {
		return fmt.Errorf("store: lock %s: %w", name, err)
	}
	return nil
}

// bindNumbered rewrites ? placeholders as $1, $2, … for Postgres. The queries
// that use it have no ? anywhere else.
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
