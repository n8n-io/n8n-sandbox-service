package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
		Kind:         KindGeneral,
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}

	got, err := s.GetAnyKind(rec.ID)
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
	got, err := s.GetAnyKind("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Egress != "public" {
		t.Fatalf("legacy egress = %q, want public", got.Egress)
	}
}

// Rows from before the code sandbox columns existed read back as general-kind
// sandboxes without a name or lifetime settings.
func TestCodeSandboxColumnsMigrationBackfillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// Roll the schema back to before the columns and plant a row there.
	if _, err := s.db.Exec(`DROP INDEX ` + codeNameIndex); err != nil {
		t.Fatalf("drop code name index: %v", err)
	}
	for _, col := range []string{"kind", "name", "options_hash", "idle_timeout_ms", "max_age_ms", "draining_since"} {
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
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.GetAnyKind("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Kind != KindGeneral || got.Name != "" || got.OptionsHash != "" ||
		got.IdleTimeoutMs != 0 || got.MaxAgeMs != 0 || got.DrainingSince != 0 {
		t.Fatalf("legacy row: %+v, want general kind and zero code sandbox fields", got)
	}
	// The migration also brings back the name index and the column checks.
	testCodeSandboxNames(t, s, "16161616-1616-1616-1616-161616161616", "17171717-1717-1717-1717-171717171717")
}

func TestCodeSandboxNames(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testCodeSandboxNames(t, s, "14141414-1414-1414-1414-141414141414", "15151515-1515-1515-1515-151515151515")
}

// testCodeSandboxNames runs against both backends; the tenants must not exist yet.
func testCodeSandboxNames(t *testing.T, s SandboxStore, tenantA, tenantB string) {
	t.Helper()
	now := time.Now().Unix()
	for _, id := range []string{tenantA, tenantB} {
		if err := s.CreateTenant(&Tenant{ID: id, Name: "t", CreatedAt: now}); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
		t.Cleanup(func() { _ = s.DeleteTenant(id) })
	}

	created := 0
	create := func(rec SandboxRecord) (*SandboxRecord, error) {
		created++
		rec.ID = fmt.Sprintf("code-%d-%s", created, tenantA)
		rec.Status, rec.CreatedAt, rec.LastActiveAt = "running", now, now
		err := s.Create(&rec)
		if err == nil {
			t.Cleanup(func() { _ = s.Delete(rec.ID) })
		}
		return &rec, err
	}
	code := func(tenantID, name string) SandboxRecord {
		return SandboxRecord{TenantID: tenantID, Kind: KindCode, Name: name, OptionsHash: "h"}
	}

	// Every field round-trips.
	first, err := create(SandboxRecord{
		TenantID: tenantA, Kind: KindCode, Name: "n1", OptionsHash: "hash-1",
		IdleTimeoutMs: 30000, MaxAgeMs: 60000, Egress: "none",
	})
	if err != nil {
		t.Fatalf("create code sandbox: %v", err)
	}
	got, err := s.GetAnyKind(first.ID)
	if err != nil || got == nil || *got != *first {
		t.Fatalf("Get: %+v err=%v, want %+v", got, err, first)
	}
	got, err = s.GetCodeSandboxByName(tenantA, "n1")
	if err != nil || got == nil || got.ID != first.ID {
		t.Fatalf("GetCodeSandboxByName: %+v err=%v, want %s", got, err, first.ID)
	}

	// The kind is required.
	if _, err := create(SandboxRecord{TenantID: tenantA}); err == nil {
		t.Fatal("create without a kind: got nil error")
	}

	if _, err := create(code(tenantA, "n1")); !errors.Is(err, ErrSandboxNameTaken) {
		t.Fatalf("same name, same tenant: got %v, want ErrSandboxNameTaken", err)
	}
	if _, err := create(code(tenantB, "n1")); err != nil {
		t.Fatalf("same name, other tenant: %v", err)
	}
	// General sandboxes do not take code sandbox names.
	if _, err := create(SandboxRecord{Kind: KindGeneral, TenantID: tenantA, Name: "n2"}); err != nil {
		t.Fatalf("general kind with a name: %v", err)
	}
	if _, err := create(SandboxRecord{Kind: KindGeneral, TenantID: tenantA, Name: "n2"}); err != nil {
		t.Fatalf("second general kind with the same name: %v", err)
	}
	if got, err := s.GetCodeSandboxByName(tenantA, "n2"); err != nil || got != nil {
		t.Fatalf("GetCodeSandboxByName(general kind): %+v err=%v, want nil", got, err)
	}
	if got, err := s.GetCodeSandboxByName(tenantA, "missing"); err != nil || got != nil {
		t.Fatalf("GetCodeSandboxByName(missing): %+v err=%v, want nil", got, err)
	}

	// A draining sandbox frees its name, and the lookup skips it.
	draining := code(tenantA, "n3")
	draining.DrainingSince = now
	drained, err := create(draining)
	if err != nil {
		t.Fatalf("create draining code sandbox: %v", err)
	}
	if got, err := s.GetCodeSandboxByName(tenantA, "n3"); err != nil || got != nil {
		t.Fatalf("GetCodeSandboxByName(draining only): %+v err=%v, want nil", got, err)
	}
	current, err := create(code(tenantA, "n3"))
	if err != nil {
		t.Fatalf("create next to a draining sandbox: %v", err)
	}
	got, err = s.GetCodeSandboxByName(tenantA, "n3")
	if err != nil || got == nil || got.ID != current.ID {
		t.Fatalf("GetCodeSandboxByName(n3): %+v err=%v, want %s, not draining %s", got, err, current.ID, drained.ID)
	}

	// A duplicate id is not a name conflict.
	dup := code(tenantA, "n4")
	dup.ID, dup.Status, dup.CreatedAt, dup.LastActiveAt = first.ID, "running", now, now
	if err := s.Create(&dup); err == nil || errors.Is(err, ErrSandboxNameTaken) {
		t.Fatalf("duplicate id: got %v, want a non-name error", err)
	}

	// Both backends hold the same limits on kind and on lengths.
	for _, tc := range []struct {
		name string
		rec  SandboxRecord
		ok   bool
	}{
		{name: "unknown kind", rec: SandboxRecord{TenantID: tenantA, Kind: "Code", Name: "k1"}},
		{name: "name at limit", rec: code(tenantA, strings.Repeat("n", 128)), ok: true},
		{name: "name too long", rec: code(tenantA, strings.Repeat("m", 129))},
		{name: "options hash at limit", rec: SandboxRecord{TenantID: tenantA, Kind: KindCode, Name: "h1", OptionsHash: strings.Repeat("h", 512)}, ok: true},
		{name: "options hash too long", rec: SandboxRecord{TenantID: tenantA, Kind: KindCode, Name: "h2", OptionsHash: strings.Repeat("h", 513)}},
	} {
		_, err := create(tc.rec)
		if tc.ok && err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.ok && (err == nil || errors.Is(err, ErrSandboxNameTaken)) {
			t.Fatalf("%s: got %v, want a constraint error", tc.name, err)
		}
	}

	// Deleting the sandbox frees its name.
	if err := s.Delete(first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := create(code(tenantA, "n1")); err != nil {
		t.Fatalf("create after delete: %v", err)
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
		Kind: KindGeneral,
	})
	must(&SandboxRecord{
		ID: "b-stop-old", Status: "stopped", CreatedAt: 2, LastActiveAt: old,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x",
		Kind: KindGeneral,
	})
	must(&SandboxRecord{
		ID: "c-run-recent", Status: "running", CreatedAt: 3, LastActiveAt: recent,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x",
		Kind: KindGeneral,
	})
	must(&SandboxRecord{
		ID: "d-eph-old", Status: "running", CreatedAt: 4, LastActiveAt: old,
		RunnerControlGRPCAddr: ctl, RunnerHTTPBase: "http://x", Ephemeral: true,
		Kind: KindGeneral,
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

	// Old running rows split by the ephemeral flag: regular ones are stop
	// candidates, ephemeral ones delete candidates.
	for _, tc := range []struct {
		ephemeral bool
		want      string
	}{{false, "a-run-old"}, {true, "d-eph-old"}} {
		stopRows, err := s.ListForIdleReapStop(cutoff, tc.ephemeral)
		if err != nil {
			t.Fatalf("ListForIdleReapStop(ephemeral=%v): %v", tc.ephemeral, err)
		}
		var stopIDs []string
		for _, r := range stopRows {
			stopIDs = append(stopIDs, r.ID)
		}
		if len(stopIDs) != 1 || stopIDs[0] != tc.want {
			t.Fatalf("stop candidates (ephemeral=%v): got %v want [%s]", tc.ephemeral, stopIDs, tc.want)
		}
	}
}

func TestRecordActivity(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	testRecordActivity(t, s, "activity-1")
}

// testRecordActivity runs against both backends; no id starting with prefix
// may exist yet.
func testRecordActivity(t *testing.T, s SandboxStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	a, b := prefix+"-a", prefix+"-b"
	for _, id := range []string{a, b} {
		if err := s.Create(&SandboxRecord{Kind: KindGeneral, ID: id, Status: "running", CreatedAt: 100, LastActiveAt: 100}); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = s.Delete(id) })
	}

	type row struct {
		lastActiveAt int64
		status       string
	}
	check := func(step string, batch map[string]int64, want map[string]row) {
		t.Helper()
		if err := s.RecordActivity(ctx, batch); err != nil {
			t.Fatalf("%s: record activity: %v", step, err)
		}
		for id, w := range want {
			got, err := s.GetAnyKind(id)
			if err != nil || got == nil {
				t.Fatalf("%s: get %s: %+v err=%v", step, id, got, err)
			}
			if got.LastActiveAt != w.lastActiveAt || got.Status != w.status {
				t.Fatalf("%s: %s last_active_at=%d status=%q, want %d %q", step, id, got.LastActiveAt, got.Status, w.lastActiveAt, w.status)
			}
		}
	}

	check("newer times, and a sandbox without a row",
		map[string]int64{a: 200, b: 300, prefix + "-missing": 400},
		map[string]row{a: {200, "running"}, b: {300, "running"}})
	// Another API pod may write an older time after a newer one.
	check("same and older times",
		map[string]int64{a: 200, b: 250},
		map[string]row{a: {200, "running"}, b: {300, "running"}})

	if err := s.UpdateStatus(a, "stopped"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	// A wake: running again, without moving the time back.
	check("older time on a stopped row",
		map[string]int64{a: 150},
		map[string]row{a: {200, "running"}, b: {300, "running"}})

	for _, status := range []string{"archived", "restoring"} {
		if err := s.UpdateStatus(b, status); err != nil {
			t.Fatalf("update status: %v", err)
		}
		// A late write must not revive a sandbox whose runner copy may be gone.
		check("newer time on a "+status+" row",
			map[string]int64{b: 500},
			map[string]row{b: {300, status}})
	}

	check("nothing", map[string]int64{}, map[string]row{a: {200, "running"}})
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
		Kind:            KindGeneral,
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
	got, err := s.GetAnyKind(rec.ID)
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
	got, err = s.GetAnyKind("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Template != "default" || got.IdleStopAfter != 0 || got.IdleDeleteAfter != 0 || got.EgressProxy != "" {
		t.Fatalf("legacy row = %+v, want the defaults", got)
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
		rec.Kind, rec.TenantID, rec.CreatedAt = KindGeneral, AdminTenantID, 1
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
		rec.Kind, rec.TenantID, rec.CreatedAt, rec.LastActiveAt = KindGeneral, AdminTenantID, old, old
		if err := s.Create(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TouchRunnerActive(prefix+"r1", []string{prefix + "a", prefix + "b", prefix + "c"}); err != nil {
		t.Fatal(err)
	}
	for id, wantTouched := range map[string]bool{"a": true, "b": false, "c": false} {
		rec, _ := s.GetAnyKind(prefix + id)
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
	if err := s.Create(&SandboxRecord{Kind: KindGeneral, ID: id, Status: "stopped", TenantID: AdminTenantID, RunnerID: prefix + "r1", RunnerControlGRPCAddr: "r1:1", IdleArchiveAfter: 600}); err != nil {
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
	rec, _ := s.GetAnyKind(id)
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
	if rec, _ := s.GetAnyKind(id); rec.Status != "restoring" {
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
	rec, _ = s.GetAnyKind(id)
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
