package registry

import (
	"errors"
	"testing"
	"time"
)

func TestRegistryPlacementUsesSlotBlockingCapacityOnly(t *testing.T) {
	reg := New(45 * time.Second)
	reg.Upsert("r1", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 2, 2, 10)

	if _, err := reg.PickRoundRobin(); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickRoundRobin() error = %v, want ErrNoRunners when used >= total", err)
	}

	reg.Upsert("r1", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 4, 2, 10)
	run, err := reg.PickRoundRobin()
	if err != nil {
		t.Fatalf("PickRoundRobin() failed: %v", err)
	}
	if run.ID != "r1" {
		t.Fatalf("runner ID = %q, want r1", run.ID)
	}
	if run.CapacityUsed != 2 || run.CapacityStopped != 10 {
		t.Fatalf("capacity = used %d stopped %d, want used 2 stopped 10", run.CapacityUsed, run.CapacityStopped)
	}
}
