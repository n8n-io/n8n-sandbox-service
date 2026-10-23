package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePersistsDockerMetadata(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	rec := &SandboxRecord{
		ID:           "sandbox-1",
		Status:       "running",
		CreatedAt:    1,
		LastActiveAt: 2,
		ContainerIP:  "172.30.0.2",
		DaemonPort:   8081,
		Ephemeral:    true,
		Egress:       "none",
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}

	got, err := s.Get(rec.ID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if got == nil {
		t.Fatal("expected record")
	}
	if got.ContainerIP != rec.ContainerIP || got.DaemonPort != rec.DaemonPort || !got.Ephemeral || got.Egress != "none" {
		t.Fatalf("unexpected docker metadata: %+v", got)
	}
}

// Rows from before the egress column existed read back as public.
func TestEgressMigrationBackfillsPublic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// Roll the schema back to before the column and plant a row there.
	if _, err := s.db.Exec(`ALTER TABLE sandboxes DROP COLUMN egress`); err != nil {
		t.Fatalf("drop egress column: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sandboxes (id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip)
		VALUES ('legacy', 'running', 1, 2, '', '', '')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = New(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s.Close()
	got, err := s.Get("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Egress != "public" {
		t.Fatalf("legacy egress = %q, want public", got.Egress)
	}
}

// The create options added for templates, idle windows and the egress proxy
// round-trip, and rows from before them read back as the defaults.
func TestCreateOptionColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	rec := &SandboxRecord{
		ID:              "with-options",
		Status:          "running",
		Egress:          "proxy",
		Template:        "python",
		IdleStopAfter:   600,
		IdleDeleteAfter: 7200,
		EgressProxy:     `{"allow":[]}`,
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}
	got, err := s.Get(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("get: got=%v err=%v", got, err)
	}
	if got.Template != "python" || got.IdleStopAfter != 600 || got.IdleDeleteAfter != 7200 || got.EgressProxy != rec.EgressProxy {
		t.Fatalf("round trip = %+v", got)
	}

	for _, col := range []string{"template", "idle_stop_after", "idle_delete_after", "egress_proxy"} {
		if _, err := s.db.Exec(`ALTER TABLE sandboxes DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s column: %v", col, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO sandboxes (id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip)
		VALUES ('legacy', 'running', 1, 2, '', '', '')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s.Close()
	got, err = s.Get("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Template != "default" || got.IdleStopAfter != 0 || got.IdleDeleteAfter != 0 || got.EgressProxy != "" {
		t.Fatalf("legacy row = %+v, want the defaults", got)
	}
}

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
	rec := &SandboxRecord{ID: "persisted", Status: "running", CreatedAt: 1, LastActiveAt: 2}
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
	got, err := s.Get(rec.ID)
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

func TestListForIdleReapDeleteAndStop(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	old := int64(100)
	recent := int64(500)
	ctl := "runner:9091"

	must := func(r *SandboxRecord) {
		t.Helper()
		if err := s.Create(r); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	must(&SandboxRecord{
		ID: "a-run-old", Status: "running", CreatedAt: 1, LastActiveAt: old,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x",
	})
	must(&SandboxRecord{
		ID: "b-stop-old", Status: "stopped", CreatedAt: 2, LastActiveAt: old,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x",
	})
	must(&SandboxRecord{
		ID: "c-run-recent", Status: "running", CreatedAt: 3, LastActiveAt: recent,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x",
	})
	must(&SandboxRecord{
		ID: "d-eph-old", Status: "running", CreatedAt: 4, LastActiveAt: old,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x", Ephemeral: true,
	})

	// Idle for more than the global window at now: last_active_at <= 200.
	const now, global = int64(300), int64(100)
	delRows, err := s.ListIdle(IdleDelete, now, global)
	if err != nil {
		t.Fatalf("ListIdle(delete): %v", err)
	}
	ids := make([]string, 0, len(delRows))
	for _, r := range delRows {
		ids = append(ids, r.ID)
	}
	// Old last_active_at: stopped row only (running rows are stop candidates).
	if len(ids) != 1 {
		t.Fatalf("delete candidates: got %d rows %v want 1 (b-stop-old)", len(ids), ids)
	}
	if ids[0] != "b-stop-old" {
		t.Fatalf("unexpected delete id %q", ids[0])
	}

	// Old running rows split by the ephemeral flag: regular ones are stop
	// candidates, ephemeral ones delete candidates.
	for _, tc := range []struct {
		phase IdlePhase
		want  string
	}{{IdleStop, "a-run-old"}, {IdleEphemeral, "d-eph-old"}} {
		stopRows, err := s.ListIdle(tc.phase, now, global)
		if err != nil {
			t.Fatalf("ListIdle(%v): %v", tc.phase, err)
		}
		var stopIDs []string
		for _, r := range stopRows {
			stopIDs = append(stopIDs, r.ID)
		}
		if len(stopIDs) != 1 || stopIDs[0] != tc.want {
			t.Fatalf("candidates (%v): got %v want [%s]", tc.phase, stopIDs, tc.want)
		}
	}
}

// A row's own window replaces the global one in both directions, and with the
// global window off only rows with their own window are candidates.
func TestListIdlePerSandboxWindows(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testListIdlePerSandboxWindows(t, s, "")
}

func testListIdlePerSandboxWindows(t *testing.T, s SandboxStore, prefix string) {
	t.Helper()
	const now = int64(10_000)
	for _, rec := range []*SandboxRecord{
		{ID: prefix + "global", LastActiveAt: now - 150},                      // global 100: due
		{ID: prefix + "longer", LastActiveAt: now - 150, IdleStopAfter: 3600}, // own 1h: not due
		{ID: prefix + "shorter", LastActiveAt: now - 70, IdleStopAfter: 60},   // own 60s: due
		{ID: prefix + "stopped", LastActiveAt: now - 150, Status: "stopped"},  // other phase
		{ID: prefix + "del", LastActiveAt: now - 150, IdleDeleteAfter: 120, Status: "stopped"},
	} {
		if rec.Status == "" {
			rec.Status = "running"
		}
		rec.TenantID, rec.CreatedAt = AdminTenantID, 1
		if err := s.Create(rec); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(phase IdlePhase, global int64) map[string]bool {
		rows, err := s.ListIdle(phase, now, global)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			if strings.HasPrefix(r.ID, prefix) {
				out[strings.TrimPrefix(r.ID, prefix)] = true
			}
		}
		return out
	}
	if got := ids(IdleStop, 100); len(got) != 2 || !got["global"] || !got["shorter"] {
		t.Fatalf("stop, global 100: %v", got)
	}
	if got := ids(IdleStop, 0); len(got) != 1 || !got["shorter"] {
		t.Fatalf("stop, global off: %v", got)
	}
	if got := ids(IdleDelete, 0); len(got) != 1 || !got["del"] {
		t.Fatalf("delete, global off: %v", got)
	}
	if got := ids(IdleDelete, 100); len(got) != 2 || !got["del"] || !got["stopped"] {
		t.Fatalf("delete, global 100: %v", got)
	}
}

func TestTenantAndAPIKeyCRUD(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	tenant := &Tenant{
		ID:   "11111111-1111-1111-1111-111111111111",
		Name: "t1", ExternalRef: "ext", MaxSandboxes: 3, CreatedAt: now,
	}
	if err := s.CreateTenant(tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	key := &APIKey{
		ID:       "22222222-2222-2222-2222-222222222222",
		TenantID: tenant.ID, KeyHash: "abc", Prefix: "deadbeef", CreatedAt: now,
	}
	if err := s.CreateAPIKey(key); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := s.Create(&SandboxRecord{
		ID: "s1", Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenant.ID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	n, err := s.CountByTenant(tenant.ID)
	if err != nil || n != 1 {
		t.Fatalf("CountByTenant: n=%d err=%v", n, err)
	}
	listed, err := s.ListByTenant(tenant.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListByTenant: len=%d err=%v", len(listed), err)
	}

	active, err := s.ListActiveAPIKeysByPrefix("deadbeef")
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActiveAPIKeysByPrefix: len=%d err=%v", len(active), err)
	}
	if err := s.RevokeAPIKey(key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	active, err = s.ListActiveAPIKeysByPrefix("deadbeef")
	if err != nil || len(active) != 0 {
		t.Fatalf("after revoke: len=%d err=%v", len(active), err)
	}
}

func TestUpdateTenantLimits(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testUpdateTenantLimits(t, s, "77777777-7777-7777-7777-777777777777")
}

// testUpdateTenantLimits runs against both backends; tenantID must not exist yet.
func testUpdateTenantLimits(t *testing.T, s SandboxStore, tenantID string) {
	t.Helper()
	now := time.Now().Unix()
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", ExternalRef: "ext", MaxSandboxes: 5, CreatedAt: now}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteTenant(tenantID) })

	sandboxID := "s-" + tenantID
	if err := s.Create(&SandboxRecord{
		ID: sandboxID, Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenantID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(sandboxID) })

	// Unchanged values still count as a match, so they are not "not found".
	for _, n := range []int{50, 50, 0} {
		if err := s.UpdateTenantLimits(tenantID, n); err != nil {
			t.Fatalf("UpdateTenantLimits(%d): %v", n, err)
		}
		got, err := s.GetTenant(tenantID)
		if err != nil || got == nil {
			t.Fatalf("GetTenant: %+v err=%v", got, err)
		}
		want := Tenant{ID: tenantID, Name: "t", ExternalRef: "ext", MaxSandboxes: n, CreatedAt: now}
		if *got != want {
			t.Fatalf("after UpdateTenantLimits(%d): got %+v, want %+v", n, *got, want)
		}
	}

	if n, err := s.CountByTenant(tenantID); err != nil || n != 1 {
		t.Fatalf("sandboxes after update: n=%d err=%v", n, err)
	}

	if err := s.UpdateTenantLimits("99999999-9999-9999-9999-999999999999", 1); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("UpdateTenantLimits(missing): got %v, want ErrTenantNotFound", err)
	}
}

func TestCreateEnforcesTenantQuota(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testCreateEnforcesTenantQuota(t, s, "12121212-1212-1212-1212-121212121212")
}

// testCreateEnforcesTenantQuota runs against both backends; tenantID must not exist yet.
func testCreateEnforcesTenantQuota(t *testing.T, s SandboxStore, tenantID string) {
	t.Helper()
	now := time.Now().Unix()
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", MaxSandboxes: 2, CreatedAt: now}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteTenant(tenantID) })

	created := 0
	create := func(owner string) error {
		created++
		id := fmt.Sprintf("quota-%d-%s", created, tenantID)
		err := s.Create(&SandboxRecord{ID: id, Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: owner})
		if err == nil {
			t.Cleanup(func() { _ = s.Delete(id) })
		}
		return err
	}
	wantCount := func(want int64) {
		t.Helper()
		if n, err := s.CountByTenant(tenantID); err != nil || n != want {
			t.Fatalf("CountByTenant: n=%d err=%v, want %d", n, err, want)
		}
	}

	for range 2 {
		if err := create(tenantID); err != nil {
			t.Fatalf("create below quota: %v", err)
		}
	}
	if err := create(tenantID); !errors.Is(err, ErrTenantQuotaExceeded) {
		t.Fatalf("create at quota: got %v, want ErrTenantQuotaExceeded", err)
	}
	wantCount(2)

	// Admin-owned sandboxes count toward no tenant and have no quota.
	if err := create(AdminTenantID); err != nil {
		t.Fatalf("create admin-owned: %v", err)
	}
	wantCount(2)

	// The limit is read when saving, so a lowered one applies at once.
	if err := s.UpdateTenantLimits(tenantID, 1); err != nil {
		t.Fatalf("lower limit: %v", err)
	}
	if err := create(tenantID); !errors.Is(err, ErrTenantQuotaExceeded) {
		t.Fatalf("create above lowered quota: got %v, want ErrTenantQuotaExceeded", err)
	}
	if err := s.UpdateTenantLimits(tenantID, 0); err != nil {
		t.Fatalf("lift limit: %v", err)
	}
	if err := create(tenantID); err != nil {
		t.Fatalf("create with unlimited quota: %v", err)
	}
	wantCount(3)
}

func TestConcurrentCreatesStopAtTenantQuota(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testConcurrentCreatesStopAtTenantQuota(t, []SandboxStore{s}, "13131313-1313-1313-1313-131313131313")
}

// testConcurrentCreatesStopAtTenantQuota saves a burst of sandboxes for one
// tenant in parallel, spread over stores (one per API pod sharing a database),
// and expects exactly the quota to be saved. tenantID must not exist yet.
func testConcurrentCreatesStopAtTenantQuota(t *testing.T, stores []SandboxStore, tenantID string) {
	t.Helper()
	const quota, creates = 3, 12
	now := time.Now().Unix()
	s := stores[0]
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", MaxSandboxes: quota, CreatedAt: now}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteTenant(tenantID) })

	start := make(chan struct{})
	errs := make([]error, creates)
	var wg sync.WaitGroup
	for i := range creates {
		id := fmt.Sprintf("burst-%d-%s", i, tenantID)
		t.Cleanup(func() { _ = s.Delete(id) })
		wg.Go(func() {
			<-start
			errs[i] = stores[i%len(stores)].Create(&SandboxRecord{
				ID: id, Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenantID,
			})
		})
	}
	close(start)
	wg.Wait()

	saved := 0
	for i, err := range errs {
		switch {
		case err == nil:
			saved++
		case !errors.Is(err, ErrTenantQuotaExceeded):
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if saved != quota {
		t.Fatalf("saved %d of %d parallel creates, want exactly the quota %d", saved, creates, quota)
	}
	if n, err := s.CountByTenant(tenantID); err != nil || n != quota {
		t.Fatalf("CountByTenant: n=%d err=%v, want %d", n, err, quota)
	}
}

func TestDeleteTenantRejectsWhenSandboxesExist(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	tenantID := "55555555-5555-5555-5555-555555555555"
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", MaxSandboxes: 5, CreatedAt: now}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.CreateAPIKey(&APIKey{
		ID:       "66666666-6666-6666-6666-666666666666",
		TenantID: tenantID, KeyHash: "h", Prefix: "cafebabe", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := s.Create(&SandboxRecord{
		ID: "s-orphan", Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenantID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := s.DeleteTenant(tenantID); !errors.Is(err, ErrTenantHasSandboxes) {
		t.Fatalf("DeleteTenant: got %v, want ErrTenantHasSandboxes", err)
	}
	if err := s.Delete("s-orphan"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if err := s.DeleteTenant(tenantID); err != nil {
		t.Fatalf("DeleteTenant after empty: %v", err)
	}
	keys, err := s.ListAPIKeysByTenant(tenantID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected keys cascaded away, got %d", len(keys))
	}
}

func TestCreateSandboxRequiresExistingTenant(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	err = s.Create(&SandboxRecord{
		ID: "s-missing-tenant", Status: "running", CreatedAt: now, LastActiveAt: now,
		TenantID: "99999999-9999-9999-9999-999999999999",
	})
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Create: got %v, want ErrTenantNotFound", err)
	}

	if err := s.Create(&SandboxRecord{
		ID: "s-admin", Status: "running", CreatedAt: now, LastActiveAt: now,
		TenantID: AdminTenantID,
	}); err != nil {
		t.Fatalf("Create admin-owned: %v", err)
	}
}

func TestDeleteTenantSerializesWithCreate(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	tenantID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", MaxSandboxes: 5, CreatedAt: now}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Hold the create transaction open (BEGIN IMMEDIATE) while delete runs on
	// another goroutine — delete must wait, then see the inserted sandbox and refuse.
	conn, err := s.beginImmediate()
	if err != nil {
		t.Fatalf("beginImmediate: %v", err)
	}
	if err := s.insertSandbox(conn, &SandboxRecord{
		ID: "s-racing", Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenantID,
	}); err != nil {
		rollbackConn(conn)
		t.Fatalf("insert in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.DeleteTenant(tenantID)
	}()

	select {
	case err := <-done:
		t.Fatalf("DeleteTenant returned before create committed: %v", err)
	case <-time.After(100 * time.Millisecond):
		// still blocked on the IMMEDIATE write lock / connection — good
	}

	if err := commitConn(conn); err != nil {
		t.Fatalf("commit create: %v", err)
	}
	err = <-done
	if !errors.Is(err, ErrTenantHasSandboxes) {
		t.Fatalf("DeleteTenant: got %v, want ErrTenantHasSandboxes", err)
	}
}

func TestCreateTenantWithAPIKeyAtomic(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	tenant := &Tenant{
		ID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Name: "acme", MaxSandboxes: 10, CreatedAt: now,
	}
	key := &APIKey{
		ID: "dddddddd-dddd-dddd-dddd-dddddddddddd", TenantID: tenant.ID,
		KeyHash: "hash", Prefix: "feedface", CreatedAt: now,
	}
	if err := s.CreateTenantWithAPIKey(tenant, key); err != nil {
		t.Fatalf("CreateTenantWithAPIKey: %v", err)
	}
	got, err := s.GetTenant(tenant.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTenant: got=%v err=%v", got, err)
	}
	keys, err := s.ListAPIKeysByTenant(tenant.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeysByTenant: len=%d err=%v", len(keys), err)
	}

	mismatchTenant := &Tenant{
		ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: "mismatch", MaxSandboxes: 1, CreatedAt: now,
	}
	mismatchKey := &APIKey{
		ID:       "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		TenantID: "00000000-0000-0000-0000-000000000000",
		KeyHash:  "x", Prefix: "badbad00", CreatedAt: now,
	}
	if err := s.CreateTenantWithAPIKey(mismatchTenant, mismatchKey); !errors.Is(err, ErrAPIKeyTenantMismatch) {
		t.Fatalf("CreateTenantWithAPIKey mismatch: got %v, want ErrAPIKeyTenantMismatch", err)
	}
	got, err = s.GetTenant(mismatchTenant.ID)
	if err != nil {
		t.Fatalf("GetTenant after mismatch: %v", err)
	}
	if got != nil {
		t.Fatalf("tenant should not be created on mismatch, got %+v", got)
	}

	orphanTenant := &Tenant{
		ID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", Name: "orphan", MaxSandboxes: 1, CreatedAt: now,
	}
	badKey := &APIKey{
		// Duplicate primary key; insert fails after tenant row and must roll back.
		ID: key.ID, TenantID: orphanTenant.ID,
		KeyHash: "x", Prefix: "badbad00", CreatedAt: now,
	}
	if err := s.CreateTenantWithAPIKey(orphanTenant, badKey); err == nil {
		t.Fatal("expected CreateTenantWithAPIKey to fail on duplicate key id")
	}
	got, err = s.GetTenant(orphanTenant.ID)
	if err != nil {
		t.Fatalf("GetTenant after failed create: %v", err)
	}
	if got != nil {
		t.Fatalf("tenant should be rolled back, got %+v", got)
	}
}

// A runner vouches only for running sandboxes stored as placed on it.
func TestTouchRunnerActive(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testTouchRunnerActive(t, s, "")
}

func testTouchRunnerActive(t *testing.T, s SandboxStore, prefix string) {
	t.Helper()
	old := time.Now().Add(-time.Hour).Unix()
	for _, rec := range []*SandboxRecord{
		{ID: prefix + "a", RunnerID: prefix + "r1", Status: "running"},
		{ID: prefix + "b", RunnerID: prefix + "r2", Status: "running"},
		{ID: prefix + "c", RunnerID: prefix + "r1", Status: "stopped"},
	} {
		rec.TenantID, rec.CreatedAt, rec.LastActiveAt = AdminTenantID, old, old
		if err := s.Create(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TouchRunnerActive(prefix+"r1", []string{prefix + "a", prefix + "b", prefix + "c"}); err != nil {
		t.Fatal(err)
	}
	for id, wantTouched := range map[string]bool{"a": true, "b": false, "c": false} {
		rec, _ := s.Get(prefix + id)
		if touched := rec.LastActiveAt > old; touched != wantTouched {
			t.Errorf("%s touched = %v, want %v", id, touched, wantTouched)
		}
	}
	if err := s.TouchRunnerActive(prefix+"r1", nil); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveLifecycle(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testArchiveLifecycle(t, s, "")
}

// archived -> restoring (once) -> running, with the runner fields and the
// archive fields moving as the archive's life says.
func testArchiveLifecycle(t *testing.T, s SandboxStore, prefix string) {
	t.Helper()
	id := prefix + "arch"
	if err := s.Create(&SandboxRecord{ID: id, Status: "stopped", TenantID: AdminTenantID, RunnerID: prefix + "r1", RunnerControlGRPCAddr: "r1:1", IdleArchiveAfter: 600}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetArchived(id, "t/s/a", "wrapped", "v1", 123, 1000); err != nil {
		t.Fatal(err)
	}
	if recs, _ := s.ListByRunner(prefix + "r1"); len(recs) != 1 {
		t.Fatalf("by runner: %d", len(recs))
	}
	if recs, _ := s.ListArchivedOnRunner(); !containsID(recs, id) {
		t.Fatal("archived copy on runner not listed")
	}
	if err := s.ClearRunner(id); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.Get(id)
	if rec.Status != "archived" || rec.ArchiveObject != "t/s/a" || rec.ArchiveSize != 123 || rec.ArchiveVersion != "v1" || rec.RunnerID != "" || rec.IdleArchiveAfter != 600 {
		t.Fatalf("archived = %+v", rec)
	}
	if won, err := s.BeginRestore(id, "archived", 0, 2000); err != nil || !won {
		t.Fatalf("first restore: %v %v", won, err)
	}
	if won, _ := s.BeginRestore(id, "archived", 0, 2000); won {
		t.Fatal("second restore also won")
	}
	// Two pods taking over the same stale restore: only one wins.
	if won, err := s.BeginRestore(id, "restoring", 2000, 2500); err != nil || !won {
		t.Fatalf("takeover: %v %v", won, err)
	}
	if won, _ := s.BeginRestore(id, "restoring", 2000, 2600); won {
		t.Fatal("second takeover also won")
	}
	// The restore that was taken over neither finishes nor aborts.
	if ok, err := s.SetRestored(id, 2000, prefix+"r3", "https://r3", "r3:1", "10.0.0.3", 2700); err != nil || ok {
		t.Fatalf("stale set restored: %v %v", ok, err)
	}
	if err := s.AbortRestore(id, 2000); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.Get(id); rec.Status != "restoring" {
		t.Fatalf("stale abort applied: %s", rec.Status)
	}
	// ClearRunner leaves a sandbox that is not archived alone.
	if err := s.ClearRunner(id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetRestored(id, 2500, prefix+"r2", "https://r2", "r2:1", "10.0.0.2", 3000); err != nil || !ok {
		t.Fatalf("set restored: %v %v", ok, err)
	}
	if err := s.ClearRunner(id); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.Get(id)
	if rec.Status != "running" || rec.RunnerID != prefix+"r2" || rec.ArchiveObject != "" || rec.ArchiveKey != "" || rec.LastActiveAt != 3000 {
		t.Fatalf("restored = %+v", rec)
	}
	_ = s.Delete(id)
}

func containsID(recs []*SandboxRecord, id string) bool {
	for _, r := range recs {
		if r.ID == id {
			return true
		}
	}
	return false
}
