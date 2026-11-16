package store

import (
	"slices"
	"testing"
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
// Methods on many sandboxes see only the rows their filter selects.
func testKindScoping(t *testing.T, s SandboxStore, tenantID string) {
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
		t.Cleanup(func() { _ = s.Delete(rec.ID) })
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

	// One sandbox is found by its id, whatever its kind.
	if got, err := s.Get(code); err != nil || got == nil || got.Kind != KindCode {
		t.Fatalf("Get(code): %+v err=%v", got, err)
	}

	recs, err := s.List(KindFilter(KindDefault))
	wantIDs("List(default)", recs, err, def)
	recs, err = s.List(ofTenant(KindCode))
	wantIDs("List(tenant, code)", recs, err, code)
	recs, err = s.List(SandboxFilter{Kinds: AllKinds(), TenantID: "other"})
	wantIDs("List(other tenant)", recs, err)
	recs, err = s.ListForIdleReapStop(200, false)
	wantIDs("ListForIdleReapStop", recs, err, def, code)
	if n, err := s.Count(ofTenant(KindDefault)); err != nil || n != 1 {
		t.Fatalf("Count(tenant, default) = %d err=%v, want 1", n, err)
	}
	if n, err := s.Count(ofTenant(AllKinds()...)); err != nil || n != 2 {
		t.Fatalf("Count(tenant, all) = %d err=%v, want 2", n, err)
	}

	// Kinds are checked, and no kinds is an error rather than every kind.
	if _, err := s.List(KindFilter("")); err == nil {
		t.Fatal("List with an empty kind: got nil error")
	}
	if _, err := s.List(SandboxFilter{}); err == nil {
		t.Fatal("List without kinds: got nil error")
	}
	if _, err := s.Count(KindFilter("Code")); err == nil {
		t.Fatal("Count with an unknown kind: got nil error")
	}
}
