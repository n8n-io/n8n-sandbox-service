package store

import (
	"errors"
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
	if got.ContainerIP != rec.ContainerIP || got.DaemonPort != rec.DaemonPort {
		t.Fatalf("unexpected docker metadata: %+v", got)
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
		t.Fatalf("foreign_keys: want 1, got %d (dsn=%q)", on, sqliteDSN(":memory:"))
	}
}

func TestSQLiteDSN(t *testing.T) {
	if got, want := sqliteDSN(":memory:"), ":memory:?_pragma=foreign_keys(1)"; got != want {
		t.Fatalf("memory dsn: got %q want %q", got, want)
	}
	if got, want := sqliteDSN("/tmp/api.db"), "/tmp/api.db?_pragma=foreign_keys(1)"; got != want {
		t.Fatalf("file dsn: got %q want %q", got, want)
	}
	if got, want := sqliteDSN("file:x.db?mode=rwc"), "file:x.db?mode=rwc&_pragma=foreign_keys(1)"; got != want {
		t.Fatalf("existing query dsn: got %q want %q", got, want)
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

	cutoff := int64(200)
	delRows, err := s.ListForIdleReapDelete(cutoff)
	if err != nil {
		t.Fatalf("ListForIdleReapDelete: %v", err)
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

	stopRows, err := s.ListForIdleReapStop(cutoff)
	if err != nil {
		t.Fatalf("ListForIdleReapStop: %v", err)
	}
	var stopIDs []string
	for _, r := range stopRows {
		stopIDs = append(stopIDs, r.ID)
	}
	if len(stopIDs) != 1 || stopIDs[0] != "a-run-old" {
		t.Fatalf("stop candidates: got %v want [a-run-old]", stopIDs)
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

	// Hold the create transaction open (tenant locked) while delete runs on another
	// goroutine — delete must wait, then see the inserted sandbox and refuse.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE tenants SET name = name WHERE id = ?`, tenantID); err != nil {
		t.Fatalf("lock tenant: %v", err)
	}
	if err := s.insertSandbox(tx, &SandboxRecord{
		ID: "s-racing", Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: tenantID,
	}); err != nil {
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
		// still blocked on the tenant row lock — good
	}

	if err := tx.Commit(); err != nil {
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

	orphanTenant := &Tenant{
		ID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", Name: "orphan", MaxSandboxes: 1, CreatedAt: now,
	}
	badKey := &APIKey{
		ID: "ffffffff-ffff-ffff-ffff-ffffffffffff",
		// FK target does not exist; insert fails after tenant row and must roll back.
		TenantID: "00000000-0000-0000-0000-000000000000",
		KeyHash:  "x", Prefix: "badbad00", CreatedAt: now,
	}
	if err := s.CreateTenantWithAPIKey(orphanTenant, badKey); err == nil {
		t.Fatal("expected CreateTenantWithAPIKey to fail on bad key FK")
	}
	got, err = s.GetTenant(orphanTenant.ID)
	if err != nil {
		t.Fatalf("GetTenant after failed create: %v", err)
	}
	if got != nil {
		t.Fatalf("tenant should be rolled back, got %+v", got)
	}
}
