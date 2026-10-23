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

func TestMemoryListReturnsCopiesOrderedByID(t *testing.T) {
	reg := NewMemory(45 * time.Second)
	reg.Upsert("r-b", "http://127.0.0.1:8081", "127.0.0.1:9092", false, 10, 4, 1)
	reg.Upsert("r-a", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 8, 2, 0)

	runners, err := reg.List()
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(runners) != 2 || runners[0].ID != "r-a" || runners[1].ID != "r-b" {
		t.Fatalf("List() = %+v, want r-a then r-b", runners)
	}
	if b := runners[1]; b.Healthy || b.CapacityTotal != 10 || b.CapacityUsed != 4 || b.CapacityStopped != 1 {
		t.Fatalf("List() r-b = %+v, want unhealthy 10/4/1", b)
	}

	runners[0].CapacityUsed = 99
	if run, _ := reg.Get("r-a"); run.CapacityUsed != 2 {
		t.Fatalf("mutating List() result changed the registry: CapacityUsed = %d", run.CapacityUsed)
	}
}

// Placement skips a cordoned runner and one without the template; a runner
// that lists no templates serves the default alone.
func TestPickForTemplateSkipsCordonedAndMissingTemplate(t *testing.T) {
	r := New(time.Minute)
	r.UpsertRunner(Runner{ID: "a", HTTPBaseURL: "https://a", Healthy: true, CapacityTotal: 10, CapacityUsed: 0, Cordoned: true})
	r.UpsertRunner(Runner{ID: "b", HTTPBaseURL: "https://b", Healthy: true, CapacityTotal: 10, CapacityUsed: 5})
	r.UpsertRunner(Runner{ID: "c", HTTPBaseURL: "https://c", Healthy: true, CapacityTotal: 10, CapacityUsed: 6, Templates: []Template{{Name: "python"}, {Name: "default"}}})

	if run, err := r.PickLowestUsed(); err != nil || run.ID != "b" {
		t.Fatalf("default: %+v %v, want b (a is cordoned)", run, err)
	}
	if run, err := r.PickForTemplate("python"); err != nil || run.ID != "c" {
		t.Fatalf("python: %+v %v, want c", run, err)
	}
	if _, err := r.PickForTemplate("go"); err != ErrNoRunners {
		t.Fatalf("go: %v, want ErrNoRunners", err)
	}
	if run, err := r.PickForTemplate("", "b"); err != nil || run.ID != "c" {
		t.Fatalf("empty template, b excluded: %+v %v, want c", run, err)
	}
}
