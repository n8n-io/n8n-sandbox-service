// Package limits caps how many requests are in progress at once per key, such
// as a tenant or a sandbox. It counts what is open right now; it is not a rate.
package limits

import "sync"

// KeyedLimiter admits up to a fixed number of holders per key and turns the
// rest away.
type KeyedLimiter struct {
	max    int
	mu     sync.Mutex
	counts map[string]int
}

// NewKeyedLimiter returns a limiter that admits max holders per key. A max of
// 0 or less admits everyone and tracks nothing.
func NewKeyedLimiter(max int) *KeyedLimiter {
	return &KeyedLimiter{max: max, counts: make(map[string]int)}
}

// TryAcquire takes a slot for key if it has one free. Call Release with the
// same key once for every true.
func (k *KeyedLimiter) TryAcquire(key string) bool {
	if k.max <= 0 {
		return true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.counts[key] >= k.max {
		return false
	}
	k.counts[key]++
	return true
}

// Release gives back a slot taken for key. A key left with no slots is
// dropped, so the limiter only remembers keys with requests in progress.
func (k *KeyedLimiter) Release(key string) {
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
