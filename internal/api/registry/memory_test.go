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

func TestPickLowestUsedSkipsExcluded(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 10, 5, 0)
	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, 10, 1, 0)
	reg.Upsert("r-tie", "http://127.0.0.1:8082", "127.0.0.1:9093", true, 10, 1, 0)

	for _, tc := range []struct {
		exclude []string
		want    string
	}{
		{exclude: []string{"r-low"}, want: "r-tie"},
		{exclude: []string{"r-low", "r-tie"}, want: "r-high"},
		{exclude: []string{"r-unknown"}, want: "r-low"},
	} {
		run, err := reg.PickLowestUsed(tc.exclude...)
		if err != nil {
			t.Fatalf("PickLowestUsed(%v) failed: %v", tc.exclude, err)
		}
		if run.ID != tc.want {
			t.Fatalf("PickLowestUsed(%v) = %q, want %q", tc.exclude, run.ID, tc.want)
		}
	}

	if _, err := reg.PickLowestUsed("r-low", "r-tie", "r-high"); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed(all) error = %v, want ErrNoRunners", err)
	}
}

func TestPickLowestUsedNoEligibleRunners(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r1", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 1, 1, 0)

	if _, err := reg.PickLowestUsed(); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed() error = %v, want ErrNoRunners", err)
	}
}
