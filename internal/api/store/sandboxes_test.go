package store

import (
	"fmt"
	"slices"
	"testing"
)

func TestSandboxFilterWhere(t *testing.T) {
	pg := func(n int) string { return fmt.Sprintf("$%d", n) }
	for _, tc := range []struct {
		f        SandboxFilter
		wantCond string
		wantArgs []any
	}{
		{KindFilter(KindCode), "id = $1 AND kind IN ($2)", []any{"id", "code"}},
		{KindFilter(KindGeneral, KindGeneral), "id = $1 AND kind IN ($2, $3)", []any{"id", "general", "general"}},
		{KindFilter(AllKinds()...), "id = $1", []any{"id"}},
		{KindFilter(KindCode, KindGeneral), "id = $1", []any{"id"}},
		{SandboxFilter{Kinds: AllKinds(), TenantID: "t"}, "id = $1 AND tenant_id = $2", []any{"id", "t"}},
		{SandboxFilter{Kinds: []Kind{KindCode}, TenantID: "t"}, "id = $1 AND kind IN ($2) AND tenant_id = $3", []any{"id", "code", "t"}},
	} {
		cond, args, err := tc.f.where([]string{"id = $1"}, []any{"id"}, pg)
		if err != nil || cond != tc.wantCond || !slices.Equal(args, tc.wantArgs) {
			t.Errorf("where(%+v) = %q %v err=%v, want %q %v", tc.f, cond, args, err, tc.wantCond, tc.wantArgs)
		}
	}
	// No condition at all is "", so callers leave out WHERE and SQLite counts
	// without a scan.
	if cond, args, err := KindFilter(AllKinds()...).where(nil, nil, pg); err != nil || cond != "" || len(args) != 0 {
		t.Errorf("where(all kinds) = %q %v err=%v, want no condition", cond, args, err)
	}
	for _, f := range []SandboxFilter{{}, KindFilter(KindGeneral, "x"), KindFilter(KindGeneral, KindCode, "")} {
		if _, _, err := f.where(nil, nil, pg); err == nil {
			t.Errorf("where(%+v): got nil error", f)
		}
	}
}

func TestKindScoping(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testKindScoping(t, s, "18181818-1818-1818-1818-181818181818", "19191919-1919-1919-1919-191919191919")
}

// testKindScoping runs against both backends; tenantA and tenantB must not
// exist yet. Each getter and filter sees only the kinds and tenants it names.
func testKindScoping(t *testing.T, s SandboxStore, tenantA, tenantB string) {
	t.Helper()
	// Each tenant has a sandbox of each kind: tenantA's are running, tenantB's
	// stopped.
	var genA, codeA, genB, codeB string
	for _, tc := range []struct {
		tenantID, status string
		gen, code        *string
	}{
		{tenantA, "running", &genA, &codeA},
		{tenantB, "stopped", &genB, &codeB},
	} {
		if err := s.CreateTenant(&Tenant{ID: tc.tenantID, Name: "t", CreatedAt: 1}); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
		t.Cleanup(func() { _ = s.DeleteTenant(tc.tenantID) })
		*tc.gen, *tc.code = "general-"+tc.tenantID, "code-"+tc.tenantID
		for _, rec := range []*SandboxRecord{
			{ID: *tc.gen, Kind: KindGeneral},
			{ID: *tc.code, Kind: KindCode, Name: "n"},
		} {
			rec.Status, rec.CreatedAt, rec.LastActiveAt, rec.TenantID = tc.status, 1, 100, tc.tenantID
			if err := s.Create(rec); err != nil {
				t.Fatalf("create %s: %v", rec.ID, err)
			}
			t.Cleanup(func() { _ = s.Delete(rec.ID) })
		}
	}

	// The Postgres database is shared with other tests, so only the rows of
	// the two tenants count.
	ids := func(recs []*SandboxRecord) []string {
		var out []string
		for _, r := range recs {
			if r.TenantID == tenantA || r.TenantID == tenantB {
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
	ofTenant := func(tenantID string, kinds ...Kind) SandboxFilter {
		return SandboxFilter{Kinds: kinds, TenantID: tenantID}
	}
	wantCount := func(what string, f SandboxFilter, want int64) {
		t.Helper()
		if n, err := s.Count(f); err != nil || n != want {
			t.Fatalf("%s = %d err=%v, want %d", what, n, err, want)
		}
	}

	// GetAnyKind finds a sandbox of any kind; the other getters only their own.
	for _, tc := range []struct {
		name string
		get  func(string) (*SandboxRecord, error)
		id   string
		want bool
	}{
		{"GetAnyKind(genA)", s.GetAnyKind, genA, true},
		{"GetAnyKind(codeA)", s.GetAnyKind, codeA, true},
		{"GetGeneralSandbox(genA)", s.GetGeneralSandbox, genA, true},
		{"GetGeneralSandbox(codeA)", s.GetGeneralSandbox, codeA, false},
		{"GetCodeSandbox(codeA)", s.GetCodeSandbox, codeA, true},
		{"GetCodeSandbox(genA)", s.GetCodeSandbox, genA, false},
	} {
		got, err := tc.get(tc.id)
		if err != nil || (got != nil) != tc.want || (got != nil && got.ID != tc.id) {
			t.Fatalf("%s: %+v err=%v, want found=%v", tc.name, got, err, tc.want)
		}
	}

	recs, err := s.List(KindFilter(KindGeneral))
	wantIDs("List(general)", recs, err, genA, genB)
	recs, err = s.List(KindFilter(AllKinds()...))
	wantIDs("List(all)", recs, err, genA, codeA, genB, codeB)
	recs, err = s.List(ofTenant(tenantA, KindCode))
	wantIDs("List(tenantA, code)", recs, err, codeA)
	recs, err = s.List(ofTenant(tenantB, AllKinds()...))
	wantIDs("List(tenantB, all)", recs, err, genB, codeB)
	wantCount("Count(tenantA, general)", ofTenant(tenantA, KindGeneral), 1)
	wantCount("Count(tenantA, all)", ofTenant(tenantA, AllKinds()...), 2)
	wantCount("Count(tenantB, code)", ofTenant(tenantB, KindCode), 1)

	// The idle reap lists cover every kind.
	recs, err = s.ListForIdleReapStop(200, false)
	wantIDs("ListForIdleReapStop", recs, err, genA, codeA)
	recs, err = s.ListForIdleReapDelete(200)
	wantIDs("ListForIdleReapDelete", recs, err, genB, codeB)

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
