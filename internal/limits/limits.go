// Package limits caps how many requests are in progress at once per key, such
// as a tenant or a sandbox. It counts what is open right now; it is not a rate.
package limits

import "sync"

// Keyed admits up to a fixed number of holders per key and turns the rest away.
type Keyed struct {
	max    int
	mu     sync.Mutex
	counts map[string]int
}

// NewKeyed returns a limiter that admits max holders per key. A max of 0 or
// less admits everyone and tracks nothing.
func NewKeyed(max int) *Keyed {
	return &Keyed{max: max, counts: make(map[string]int)}
}

// TryAcquire takes a slot for key if it has one free. Call Release with the
// same key once for every true.
func (k *Keyed) TryAcquire(key string) bool {
	return k.tryAcquire(key, 0)
}

// TryAcquireReserve is TryAcquire allowed up to reserve slots over the cap,
// for requests that have to get through while ordinary ones are turned away.
// The reserve is bounded, so those requests cannot be used to go around the
// cap without limit.
func (k *Keyed) TryAcquireReserve(key string, reserve int) bool {
	return k.tryAcquire(key, reserve)
}

func (k *Keyed) tryAcquire(key string, reserve int) bool {
	if k.max <= 0 {
		return true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.counts[key] >= k.max+reserve {
		return false
	}
	k.counts[key]++
	return true
}

// Release gives back a slot taken for key. A key left with no slots is
// dropped, so the limiter only remembers keys with requests in progress.
func (k *Keyed) Release(key string) {
	if k.max <= 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if n := k.counts[key]; n > 1 {
		k.counts[key] = n - 1
	} else {
		delete(k.counts, key)
	}
}
