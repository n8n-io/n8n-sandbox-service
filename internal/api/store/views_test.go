package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestKindScoping(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testKindScoping(t, s, "18181818-1818-1818-1818-181818181818")
}

// testKindScoping runs against both backends; tenantID must not exist yet.
// A row of another kind is as good as missing to every sandbox method.
func testKindScoping(t *testing.T, s Persistence, tenantID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTenant(&Tenant{ID: tenantID, Name: "t", CreatedAt: 1}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteTenant(tenantID) })

	def, code := "default-"+tenantID, "code-"+tenantID
	for _, rec := range []*SandboxRecord{
		{ID: def, Kind: KindDefault, Status: "running", CreatedAt: 1, LastActiveAt: 100, TenantID: tenantID},
		{ID: code, Kind: KindCode, Name: "n", Status: "running", CreatedAt: 1, LastActiveAt: 100, TenantID: tenantID},
	} {
		if err := s.Create(rec); err != nil {
			t.Fatalf("create %s: %v", rec.ID, err)
		}
		t.Cleanup(func() { _ = s.DeleteByIDAndKind(rec.ID, rec.Kind) })
	}

	ids := func(recs []*SandboxRecord) []string {
		var out []string
		for _, r := range recs {
			if r.TenantID == tenantID {
				out = append(out, r.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	wantIDs := func(what string, recs []*SandboxRecord, err error, want ...string) {
		t.Helper()
		slices.Sort(want)
		if err != nil || !slices.Equal(ids(recs), want) {
			t.Fatalf("%s: %v err=%v, want %v", what, ids(recs), err, want)
		}
	}

	if got, err := s.GetByIDAndKind(code, KindDefault); err != nil || got != nil {
		t.Fatalf("GetByIDAndKind(code, default): %+v err=%v, want nil", got, err)
	}
	if got, err := s.GetByIDAndKind(code, KindCode); err != nil || got == nil || got.Kind != KindCode {
		t.Fatalf("GetByIDAndKind(code, code): %+v err=%v", got, err)
	}
	for _, tc := range []struct {
		kinds []Kind
		want  bool
	}{{[]Kind{KindDefault}, false}, {[]Kind{KindCode}, true}, {AllKinds(), true}} {
		if got, err := s.ExistsByIDAndKinds(code, tc.kinds); err != nil || got != tc.want {
			t.Fatalf("ExistsByIDAndKinds(code, %v) = %v err=%v, want %v", tc.kinds, got, err, tc.want)
		}
	}

	recs, err := s.ListByKind(KindDefault)
	wantIDs("ListByKind(default)", recs, err, def)
	recs, err = s.ListByTenantAndKind(tenantID, KindCode)
	wantIDs("ListByTenantAndKind(code)", recs, err, code)
	recs, err = s.ListForIdleReapStopByKinds([]Kind{KindCode}, 200, false)
	wantIDs("ListForIdleReapStopByKinds(code)", recs, err, code)
	recs, err = s.ListForIdleReapStopByKinds(AllKinds(), 200, false)
	wantIDs("ListForIdleReapStopByKinds(all)", recs, err, def, code)
	if n, err := s.CountByTenantAndKinds(tenantID, []Kind{KindDefault}); err != nil || n != 1 {
		t.Fatalf("CountByTenantAndKinds(default) = %d err=%v, want 1", n, err)
	}
	if n, err := s.CountByTenantAndKinds(tenantID, AllKinds()); err != nil || n != 2 {
		t.Fatalf("CountByTenantAndKinds(all) = %d err=%v, want 2", n, err)
	}

	// Writes through the wrong kind change nothing.
	if err := s.UpdateStatusByIDAndKind(code, KindDefault, "stopped"); err != nil {
		t.Fatalf("UpdateStatusByIDAndKind(code, default): %v", err)
	}
	if err := s.RecordActivityByKind(ctx, KindDefault, map[string]int64{code: 500}); err != nil {
		t.Fatalf("RecordActivityByKind(default, code): %v", err)
	}
	if err := s.DeleteByIDAndKind(code, KindDefault); err != nil {
		t.Fatalf("DeleteByIDAndKind(code, default): %v", err)
	}
	if got, err := s.GetByIDAndKind(code, KindCode); err != nil || got == nil || got.Status != "running" || got.LastActiveAt != 100 {
		t.Fatalf("code sandbox after writes through the default kind: %+v err=%v", got, err)
	}

	// Kinds are checked, and no kinds is an error rather than every kind.
	if _, err := s.GetByIDAndKind(code, ""); err == nil {
		t.Fatal("GetByIDAndKind with an empty kind: got nil error")
	}
	if _, err := s.ListForIdleReapDeleteByKinds(nil, 200); err == nil {
		t.Fatal("ListForIdleReapDeleteByKinds without kinds: got nil error")
	}
	if _, err := s.CountByKinds([]Kind{"Code"}); err == nil {
		t.Fatal("CountByKinds with an unknown kind: got nil error")
	}
}

func TestSandboxStoreViews(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	defaults, codes := NewDefaultSandboxStore(s), NewCodeSandboxStore(s)
	now := time.Now().Unix()

	if err := defaults.Create(&SandboxRecord{ID: "c1", Kind: KindCode, Status: "running", CreatedAt: now, LastActiveAt: now}); err == nil {
		t.Fatal("default store created a code sandbox")
	}
	if err := codes.Create(&SandboxRecord{ID: "c1", Kind: KindCode, Name: "n", TenantID: AdminTenantID, Status: "running", CreatedAt: now, LastActiveAt: now}); err != nil {
		t.Fatalf("code store create: %v", err)
	}

	if got, err := defaults.Get("c1"); err != nil || got != nil {
		t.Fatalf("default store Get(code sandbox): %+v err=%v, want nil", got, err)
	}
	if recs, err := defaults.List(); err != nil || len(recs) != 0 {
		t.Fatalf("default store List: %d err=%v, want none", len(recs), err)
	}
	if taken, err := defaults.IDTaken("c1"); err != nil || !taken {
		t.Fatalf("default store IDTaken(code sandbox) = %v err=%v, want true", taken, err)
	}
	if got, err := codes.GetByName(AdminTenantID, "n"); err != nil || got == nil || got.ID != "c1" {
		t.Fatalf("code store GetByName: %+v err=%v", got, err)
	}

	sweeper := NewSweeperStore(s, []Kind{KindDefault})
	if recs, err := sweeper.ListForIdleReapStop(now+1, false); err != nil || len(recs) != 0 {
		t.Fatalf("default sweeper listed %d sandboxes err=%v, want none", len(recs), err)
	}
	if _, err := sweeper.GetByIDAndKind("c1", KindCode); err == nil {
		t.Fatal("default sweeper read a code sandbox")
	}
	if err := sweeper.DeleteByIDAndKind("c1", KindCode); err == nil {
		t.Fatal("default sweeper deleted a code sandbox")
	}

	if err := defaults.Delete("c1"); err != nil {
		t.Fatalf("default store Delete(code sandbox): %v", err)
	}
	if got, err := codes.Get("c1"); err != nil || got == nil {
		t.Fatalf("code sandbox after a default store delete: %+v err=%v", got, err)
	}
}
