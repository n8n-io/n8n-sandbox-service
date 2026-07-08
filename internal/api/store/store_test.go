package store

import (
	"testing"
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
