package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

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
		Kind: KindGeneral,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	n, err := s.Count(SandboxFilter{TenantID: tenant.ID, Kinds: AllKinds()})
	if err != nil || n != 1 {
		t.Fatalf("CountByTenant: n=%d err=%v", n, err)
	}
	listed, err := s.List(SandboxFilter{TenantID: tenant.ID, Kinds: []Kind{KindGeneral}})
	if err != nil || len(listed) != 1 {
		t.Fatalf("List(tenant): len=%d err=%v", len(listed), err)
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
		Kind: KindGeneral,
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

	if n, err := s.Count(SandboxFilter{TenantID: tenantID, Kinds: AllKinds()}); err != nil || n != 1 {
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
		err := s.Create(&SandboxRecord{Kind: KindGeneral, ID: id, Status: "running", CreatedAt: now, LastActiveAt: now, TenantID: owner})
		if err == nil {
			t.Cleanup(func() { _ = s.Delete(id) })
		}
		return err
	}
	wantCount := func(want int64) {
		t.Helper()
		if n, err := s.Count(SandboxFilter{TenantID: tenantID, Kinds: AllKinds()}); err != nil || n != want {
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
				Kind: KindGeneral,
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
	if n, err := s.Count(SandboxFilter{TenantID: tenantID, Kinds: AllKinds()}); err != nil || n != quota {
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
		Kind: KindGeneral,
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
		Kind:     KindGeneral,
	})
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Create: got %v, want ErrTenantNotFound", err)
	}

	if err := s.Create(&SandboxRecord{
		ID: "s-admin", Status: "running", CreatedAt: now, LastActiveAt: now,
		TenantID: AdminTenantID,
		Kind:     KindGeneral,
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
		Kind: KindGeneral,
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
