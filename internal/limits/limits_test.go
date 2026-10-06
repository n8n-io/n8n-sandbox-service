package limits

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The cap has to hold under contention, not just in sequence: a check-then-add
// would let a burst of callers all pass the check.
func TestKeyedLimiterNeverAdmitsMoreThanCapConcurrently(t *testing.T) {
	const max = 5
	k := NewKeyedLimiter(max)
	var holders, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !k.TryAcquire("a") {
				return
			}
			n := holders.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			holders.Add(-1)
			k.Release("a")
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > max {
		t.Fatalf("peak holders = %d, want at most %d", p, max)
	}
	if len(k.counts) != 0 {
		t.Fatalf("counts after all released = %v, want empty", k.counts)
	}
}

func TestKeyedLimiterCapsEachKeySeparately(t *testing.T) {
	k := NewKeyedLimiter(1)
	if !k.TryAcquire("a") {
		t.Fatal("expected the first acquire for a to succeed")
	}
	if k.TryAcquire("a") {
		t.Fatal("expected the second acquire for a to be turned away")
	}
	if !k.TryAcquire("b") {
		t.Fatal("a full key must not affect another key")
	}
	k.Release("a")
	if !k.TryAcquire("a") {
		t.Fatal("expected a released slot for a to be available again")
	}
}

// Keys are sandbox and tenant IDs, which come and go; a limiter that kept every
// key it ever saw would grow for the life of the process.
func TestKeyedLimiterForgetsKeysWithNothingInProgress(t *testing.T) {
	k := NewKeyedLimiter(3)
	k.TryAcquire("a")
	k.TryAcquire("a")
	k.TryAcquire("b")
	k.Release("a")
	k.Release("b")
	if _, ok := k.counts["b"]; ok {
		t.Error("key b is still tracked with nothing in progress")
	}
	if got := k.counts["a"]; got != 1 {
		t.Errorf("counts[a] = %d, want 1", got)
	}
	k.Release("a")
	if len(k.counts) != 0 {
		t.Errorf("counts = %v, want empty", k.counts)
	}
}

func TestKeyedLimiterWithoutCapTracksNothing(t *testing.T) {
	k := NewKeyedLimiter(0)
	for i := 0; i < 10; i++ {
		if !k.TryAcquire("a") {
			t.Fatal("turned away with no cap")
		}
	}
	k.Release("a")
	if len(k.counts) != 0 {
		t.Errorf("counts = %v, want empty with no cap", k.counts)
	}
}
