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
