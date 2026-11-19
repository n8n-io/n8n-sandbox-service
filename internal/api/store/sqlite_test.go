package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteDSNEnablesForeignKeys(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	var on int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys: want 1, got %d (dsn=%q)", on, sqliteDSN(":memory:", false))
	}
}

func TestSQLiteDSN(t *testing.T) {
	const fk = "_pragma=foreign_keys(1)"
	const walPragmas = fk + "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	const rollbackPragmas = fk + "&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)"
	for _, tc := range []struct {
		path string
		wal  bool
		want string
	}{
		{path: ":memory:", wal: false, want: ":memory:?" + fk},
		{path: ":memory:", wal: true, want: ":memory:?" + fk},
		{path: "/tmp/api.db", wal: false, want: "/tmp/api.db?" + rollbackPragmas},
		{path: "/tmp/api.db", wal: true, want: "/tmp/api.db?" + walPragmas},
		{path: "file:x.db?mode=rwc", wal: false, want: "file:x.db?mode=rwc&" + rollbackPragmas},
		{path: "file:x.db?mode=rwc", wal: true, want: "file:x.db?mode=rwc&" + walPragmas},
	} {
		if got := sqliteDSN(tc.path, tc.wal); got != tc.want {
			t.Errorf("sqliteDSN(%q, %v) = %q, want %q", tc.path, tc.wal, got, tc.want)
		}
	}
}

func TestNewSQLiteFailsWhenJournalModeIsNotApplied(t *testing.T) {
	// The URI spelling bypasses the ":memory:" exception, but SQLite still
	// keeps journal_mode=memory rather than applying the requested file mode.
	for _, tc := range []struct {
		mode string
		wal  bool
	}{
		{mode: "WAL", wal: true},
		{mode: "DELETE", wal: false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s, err := NewSQLite("file::memory:", tc.wal)
			if err == nil {
				_ = s.Close()
				t.Fatalf("NewSQLite succeeded, want %s error", tc.mode)
			}
			if !strings.Contains(err.Error(), tc.mode+" mode requested") {
				t.Fatalf("error = %v, want %s error", err, tc.mode)
			}
		})
	}
}

func TestSQLiteFileWithoutWALUsesRollbackJournal(t *testing.T) {
	s, err := NewSQLite(filepath.Join(t.TempDir(), "api.db"), false)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if mode != "delete" {
		t.Errorf("journal_mode = %q, want delete", mode)
	}
}

func TestSQLiteDisablesWALAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	s, err := NewSQLite(path, true)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	rec := &SandboxRecord{Kind: KindDefault, ID: "persisted", Status: "running", CreatedAt: 1, LastActiveAt: 2}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = NewSQLite(path, false)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if mode != "delete" {
		t.Errorf("journal_mode = %q, want delete", mode)
	}
	var sync int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	if sync != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
	got, err := s.GetAnyKind(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("get persisted record: got=%v err=%v", got, err)
	}
	if got.Status != rec.Status {
		t.Errorf("status = %q, want %q", got.Status, rec.Status)
	}
	if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
		t.Fatalf("write after disabling WAL: %v", err)
	}
}

func TestSQLiteFileUsesWALWithNormalSync(t *testing.T) {
	s, err := NewSQLite(filepath.Join(t.TempDir(), "api.db"), true)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	// 1 is NORMAL.
	var sync int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	if sync != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
}
