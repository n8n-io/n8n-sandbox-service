package obs

import (
	"context"
	"sync"
	"testing"
)

func TestFieldsCollectedThroughContext(t *testing.T) {
	ctx, fields := WithFields(context.Background())

	// Simulates a handler deeper in the chain, which only has the context.
	FieldsFrom(ctx).Add("sandbox_id", "abc")
	FieldsFrom(ctx).Add("ttfb_ms", int64(12))

	got := fields.Attrs()
	want := []any{"sandbox_id", "abc", "ttfb_ms", int64(12)}
	if len(got) != len(want) {
		t.Fatalf("Attrs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Attrs()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFieldsWithoutContextIsNoOp(t *testing.T) {
	FieldsFrom(context.Background()).Add("sandbox_id", "abc")
	if got := FieldsFrom(context.Background()).Attrs(); got != nil {
		t.Fatalf("Attrs() = %v, want nil", got)
	}
}

func TestFieldsAttrsIsACopy(t *testing.T) {
	_, fields := WithFields(context.Background())
	fields.Add("sandbox_id", "abc")

	attrs := fields.Attrs()
	attrs[1] = "mutated"

	if got := fields.Attrs(); got[1] != "abc" {
		t.Fatalf("Attrs() = %v, want the caller's mutation not to leak back", got)
	}
}

func TestFieldsConcurrentAdd(t *testing.T) {
	ctx, fields := WithFields(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			FieldsFrom(ctx).Add("key", "value")
		}()
	}
	wg.Wait()

	if got := len(fields.Attrs()); got != 16 {
		t.Fatalf("len(Attrs()) = %d, want 16", got)
	}
}
