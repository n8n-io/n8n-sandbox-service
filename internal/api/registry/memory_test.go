package registry

import (
	"errors"
	"testing"
	"time"
)

func TestPickLowestUsedSelectsLowestCapacity(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, false, 10, 5, 0)
	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, false, 10, 1, 0)
	reg.Upsert("r-tie", "http://127.0.0.1:8082", "127.0.0.1:9093", true, false, 10, 1, 0)

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
	reg.Upsert("r-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, false, 10, 5, 0)
	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, false, 10, 1, 0)
	reg.Upsert("r-tie", "http://127.0.0.1:8082", "127.0.0.1:9093", true, false, 10, 1, 0)

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

func TestPickLowestUsedSkipsCordoned(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, false, 10, 5, 0)
	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, true, 10, 1, 0)

	run, err := reg.PickLowestUsed()
	if err != nil {
		t.Fatalf("PickLowestUsed() failed: %v", err)
	}
	if run.ID != "r-high" {
		t.Fatalf("PickLowestUsed() = %q, want r-high (r-low is cordoned)", run.ID)
	}
	if _, err := reg.PickLowestUsed("r-high"); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed(r-high) error = %v, want ErrNoRunners", err)
	}

	reg.Upsert("r-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, false, 10, 1, 0)
	if run, err := reg.PickLowestUsed(); err != nil || run.ID != "r-low" {
		t.Fatalf("PickLowestUsed() after uncordon = %v, %v; want r-low", run, err)
	}
}

func TestPickLowestUsedNoEligibleRunners(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r1", "http://127.0.0.1:8080", "127.0.0.1:9091", true, false, 1, 1, 0)

	if _, err := reg.PickLowestUsed(); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed() error = %v, want ErrNoRunners", err)
	}
}

func TestMemoryListReturnsCopiesOrderedByID(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-b", "http://127.0.0.1:8081", "127.0.0.1:9092", false, true, 10, 4, 1)
	reg.Upsert("r-a", "http://127.0.0.1:8080", "127.0.0.1:9091", true, false, 8, 2, 0)

	runners, err := reg.List()
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(runners) != 2 || runners[0].ID != "r-a" || runners[1].ID != "r-b" {
		t.Fatalf("List() = %+v, want r-a then r-b", runners)
	}
	if b := runners[1]; b.Healthy || !b.Cordoned || b.CapacityTotal != 10 || b.CapacityUsed != 4 || b.CapacityStopped != 1 {
		t.Fatalf("List() r-b = %+v, want unhealthy, cordoned, 10/4/1", b)
	}
	if runners[0].Cordoned {
		t.Fatalf("List() r-a = %+v, want not cordoned", runners[0])
	}

	runners[0].CapacityUsed = 99
	if run, _ := reg.Get("r-a"); run.CapacityUsed != 2 {
		t.Fatalf("mutating List() result changed the registry: CapacityUsed = %d", run.CapacityUsed)
	}
}
