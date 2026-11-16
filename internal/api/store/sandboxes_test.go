package store

import (
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
// A row the filter does not select is as good as missing to every sandbox
// method.
func testKindScoping(t *testing.T, s Store, tenantID string) {
	t.Helper()
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
		t.Cleanup(func() { _ = s.Delete(rec.ID, KindFilter(rec.Kind)) })
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
	ofTenant := func(kinds ...Kind) SandboxFilter { return SandboxFilter{Kinds: kinds, TenantID: tenantID} }

	if got, err := s.Get(code, KindFilter(KindDefault)); err != nil || got != nil {
		t.Fatalf("Get(code, default): %+v err=%v, want nil", got, err)
	}
	if got, err := s.Get(code, KindFilter(KindCode)); err != nil || got == nil || got.Kind != KindCode {
		t.Fatalf("Get(code, code): %+v err=%v", got, err)
	}
	if got, err := s.Get(code, SandboxFilter{Kinds: AllKinds(), TenantID: "other"}); err != nil || got != nil {
		t.Fatalf("Get(code) of another tenant: %+v err=%v, want nil", got, err)
	}

	recs, err := s.List(KindFilter(KindDefault))
	wantIDs("List(default)", recs, err, def)
	recs, err = s.List(ofTenant(KindCode))
	wantIDs("List(tenant, code)", recs, err, code)
	recs, err = s.ListForIdleReapStop(KindFilter(KindCode), 200, false)
	wantIDs("ListForIdleReapStop(code)", recs, err, code)
	recs, err = s.ListForIdleReapStop(KindFilter(AllKinds()...), 200, false)
	wantIDs("ListForIdleReapStop(all)", recs, err, def, code)
	if n, err := s.Count(ofTenant(KindDefault)); err != nil || n != 1 {
		t.Fatalf("Count(tenant, default) = %d err=%v, want 1", n, err)
	}
	if n, err := s.Count(ofTenant(AllKinds()...)); err != nil || n != 2 {
		t.Fatalf("Count(tenant, all) = %d err=%v, want 2", n, err)
	}

	// Writes through a filter that does not select the row change nothing.
	if err := s.UpdateStatus(code, KindFilter(KindDefault), "stopped"); err != nil {
		t.Fatalf("UpdateStatus(code, default): %v", err)
	}
	if err := s.Delete(code, KindFilter(KindDefault)); err != nil {
		t.Fatalf("Delete(code, default): %v", err)
	}
	if err := s.Delete(code, SandboxFilter{Kinds: AllKinds(), TenantID: "other"}); err != nil {
		t.Fatalf("Delete(code) of another tenant: %v", err)
	}
	if got, err := s.Get(code, KindFilter(KindCode)); err != nil || got == nil || got.Status != "running" || got.LastActiveAt != 100 {
		t.Fatalf("code sandbox after writes that do not select it: %+v err=%v", got, err)
	}

	// Kinds are checked, and no kinds is an error rather than every kind.
	if _, err := s.Get(code, KindFilter("")); err == nil {
		t.Fatal("Get with an empty kind: got nil error")
	}
	if _, err := s.ListForIdleReapDelete(SandboxFilter{}, 200); err == nil {
		t.Fatal("ListForIdleReapDelete without kinds: got nil error")
	}
	if _, err := s.Count(KindFilter("Code")); err == nil {
		t.Fatal("Count with an unknown kind: got nil error")
	}
}

func TestKindStores(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	defaults, codes := NewDefaultSandboxStore(s), NewCodeSandboxStore(s)
	now := time.Now().Unix()

	// Each store saves the record as its own kind.
	if err := codes.Create(&SandboxRecord{ID: "c1", Kind: KindDefault, Name: "n", TenantID: AdminTenantID, Status: "running", CreatedAt: now, LastActiveAt: now}); err != nil {
		t.Fatalf("code store create: %v", err)
	}
	if got, err := s.Get("c1", KindFilter(KindCode)); err != nil || got == nil {
		t.Fatalf("code store saved %+v err=%v, want a code sandbox", got, err)
	}
	d1 := &SandboxRecord{ID: "d1", TenantID: AdminTenantID, Status: "running", CreatedAt: now, LastActiveAt: now}
	if err := defaults.Create(d1); err != nil || d1.Kind != KindDefault {
		t.Fatalf("default store create: kind %q err=%v", d1.Kind, err)
	}

	if got, err := defaults.Get("c1"); err != nil || got != nil {
		t.Fatalf("default store Get(code sandbox): %+v err=%v, want nil", got, err)
	}
	if recs, err := defaults.List(); err != nil || len(recs) != 1 || recs[0].ID != "d1" {
		t.Fatalf("default store List: %d err=%v, want d1", len(recs), err)
	}
	if _, err := defaults.ListByTenant(""); err == nil {
		t.Fatal("default store ListByTenant without a tenant: got nil error")
	}
	if taken, err := defaults.IDTaken("c1"); err != nil || !taken {
		t.Fatalf("default store IDTaken(code sandbox) = %v err=%v, want true", taken, err)
	}
	if got, err := codes.GetByName(AdminTenantID, "n"); err != nil || got == nil || got.ID != "c1" {
		t.Fatalf("code store GetByName: %+v err=%v", got, err)
	}

	if err := defaults.Delete("c1"); err != nil {
		t.Fatalf("default store Delete(code sandbox): %v", err)
	}
	if got, err := codes.Get("c1"); err != nil || got == nil {
		t.Fatalf("code sandbox after a default store delete: %+v err=%v", got, err)
	}
}
