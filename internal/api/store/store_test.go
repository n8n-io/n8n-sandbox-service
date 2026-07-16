package store

import (
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
