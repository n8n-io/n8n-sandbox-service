package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	_ "modernc.org/sqlite" // register the "sqlite" driver
)

var errJournalMode = errors.New("store: unexpected journal_mode")

// SQLiteStore wraps a *sql.DB and exposes CRUD operations for SandboxRecord rows.
type SQLiteStore struct {
	db           *sql.DB
	sandboxLocks [64]chan struct{}
}

// NewSQLite opens (or creates) the SQLite database at dbPath, runs schema migrations,
// and returns a ready store. Use ":memory:" for an in-process ephemeral database.
// A database file uses WAL with synchronous=NORMAL when useWal is set, or DELETE
// with synchronous=FULL otherwise. ":memory:" ignores the setting.
func NewSQLite(dbPath string, useWal bool) (*SQLiteStore, error) {
	useWal = useWal && dbPath != ":memory:"
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, useWal))
	if err != nil {
		return nil, fmt.Errorf("store: open db %s: %w", dbPath, err)
	}

	// SQLite performs best with a single writer connection; cap the pool.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping db: %w", err)
	}

	if dbPath != ":memory:" {
		wantMode := "DELETE"
		if useWal {
			wantMode = "WAL"
		}
		var mode string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: read journal_mode: %w", err)
		}
		if !strings.EqualFold(mode, wantMode) {
			_ = db.Close()
			return nil, fmt.Errorf("%w: %s mode requested, got %q", errJournalMode, wantMode, mode)
		}
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: run migrations: %w", err)
	}

	for _, stmt := range []struct {
		sql  string
		name string
	}{
		{sql: sqliteAddContainerIPCol, name: "container_ip"},
		{sql: sqliteAddDaemonPortCol, name: "daemon_port"},
		{sql: sqliteDropContainerIDCol, name: "drop_container_id"},
		{sql: sqliteAddRunnerIDCol, name: "runner_id"},
		{sql: sqliteAddRunnerHTTPBaseURLCol, name: "runner_http_base_url"},
		{sql: sqliteAddRunnerControlGRPCAddrCol, name: "runner_control_grpc_addr"},
		{sql: sqliteAddTenantIDCol, name: "tenant_id"},
		{sql: sqliteAddEphemeralCol, name: "ephemeral"},
		{sql: sqliteAddEgressCol, name: "egress"},
		{sql: sqliteAddKindCol, name: "kind"},
		{sql: sqliteAddNameCol, name: "name"},
		{sql: sqliteAddOptionsHashCol, name: "options_hash"},
		{sql: sqliteAddIdleTimeoutMsCol, name: "idle_timeout_ms"},
		{sql: sqliteAddMaxAgeMsCol, name: "max_age_ms"},
		{sql: sqliteAddDrainingSinceCol, name: "draining_since"},
	} {
		if _, err := db.Exec(stmt.sql); err != nil {
			if strings.Contains(err.Error(), "duplicate column") ||
				strings.Contains(err.Error(), "no such column") {
				continue
			}
			_ = db.Close()
			return nil, fmt.Errorf("store: migration %s: %w", stmt.name, err)
		}
	}

	if _, err := db.Exec(sqliteSandboxCodeNameIndex); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sandboxes code name index: %w", err)
	}

	if _, err := db.Exec(sqliteBackfillAdminTenantID); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: backfill admin tenant_id: %w", err)
	}

	if _, err := db.Exec(sqliteTenantsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: tenants schema: %w", err)
	}

	if _, err := db.Exec(sqliteSharedVolumesSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: shared volumes schema: %w", err)
	}

	slog.Debug("store: opened sqlite database", "path", dbPath)
	store := &SQLiteStore{db: db}
	for i := range store.sandboxLocks {
		store.sandboxLocks[i] = make(chan struct{}, 1)
	}
	return store, nil
}

// sqliteDSN adds modernc _pragma flags so every pooled connection gets them.
// Connection-local flags must be applied on every connection.
func sqliteDSN(dbPath string, useWal bool) string {
	pragmas := "_pragma=foreign_keys(1)"
	if dbPath == ":memory:" {
		return ":memory:?" + pragmas
	}
	if useWal {
		pragmas += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	} else {
		// WAL persists in the database file, so disabling it needs an explicit switch.
		pragmas += "&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)"
	}
	if strings.Contains(dbPath, "?") {
		return dbPath + "&" + pragmas
	}
	return dbPath + "?" + pragmas
}

// New opens SQLite at dbPath without WAL. It is a shorthand for NewSQLite.
func New(dbPath string) (*SQLiteStore, error) {
	return NewSQLite(dbPath, false)
}

func (s *SQLiteStore) Backend() Backend { return BackendSQLite }

func (s *SQLiteStore) Close() error { return s.db.Close() }

// beginImmediate starts a transaction with a RESERVED write lock (BEGIN IMMEDIATE).
// Caller must Commit or Rollback, then Close the conn.
func (s *SQLiteStore) beginImmediate() (*sql.Conn, error) {
	return s.beginImmediateContext(context.Background())
}

// beginImmediateContext is beginImmediate that stops waiting for the pool's
// connection when ctx ends. BEGIN itself ignores ctx: the driver can report a
// cancellation after the transaction began, and the connection would go back
// to the pool with it open.
func (s *SQLiteStore) beginImmediateContext(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func commitConn(conn *sql.Conn) error {
	_, err := conn.ExecContext(context.Background(), "COMMIT")
	cerr := conn.Close()
	if err != nil {
		return err
	}
	return cerr
}

func rollbackConn(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()
}
