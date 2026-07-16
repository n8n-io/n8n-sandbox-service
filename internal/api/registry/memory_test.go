package registry

import (
	"errors"
	"testing"
	"time"
)

func TestPickLowestUsedSelectsLowestCapacity(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 10, 5, 0)
	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, 10, 1, 0)
	reg.Upsert("r-tie", "http://127.0.0.1:8082", "127.0.0.1:9093", true, 10, 1, 0)

	run, err := reg.PickLowestUsed()
	if err != nil {
		t.Fatalf("PickLowestUsed() failed: %v", err)
	}
	if run.ID != "r-low" {
		t.Fatalf("PickLowestUsed() = %q, want r-low (lowest id at equal capacity)", run.ID)
	}
}

func TestPickLowestUsedNoEligibleRunners(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r1", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 1, 1, 0)

	if _, err := reg.PickLowestUsed(); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed() error = %v, want ErrNoRunners", err)
	}
}
