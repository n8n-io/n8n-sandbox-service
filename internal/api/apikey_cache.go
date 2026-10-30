package api

import (
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// maxAPIKeyCacheEntries bounds the cache, which starts over when full, so
// made-up keys cannot grow it.
const maxAPIKeyCacheEntries = 10_000

// apiKeyCache remembers tenant key lookups; a nil cache remembers nothing.
// How: "Tenant key cache" in docs/architecture.md.
type apiKeyCache struct {
	ttl   time.Duration
	now   func() time.Time
	group singleflight.Group

	mu      sync.Mutex
	entries map[string]apiKeyCacheEntry
	gen     uint64 // changed by every invalidation
}

type apiKeyCacheEntry struct {
	tenantID string // "" = no such key
	expires  time.Time
}

func newAPIKeyCache(ttl time.Duration) *apiKeyCache {
	if ttl <= 0 {
		return nil
	}
	return &apiKeyCache{ttl: ttl, now: time.Now, entries: make(map[string]apiKeyCacheEntry)}
}

func (c *apiKeyCache) lookup(keyHash string, load func() (string, error)) (string, error) {
	if c == nil {
		return load()
	}
	c.mu.Lock()
	e, ok := c.entries[keyHash]
	gen := c.gen
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		return e.tenantID, nil
	}

	// Keyed by gen, so a lookup started after a revoke neither waits for nor
	// caches the answer of one started before it.
	v, err, _ := c.group.Do(strconv.FormatUint(gen, 10)+":"+keyHash, func() (any, error) {
		tenantID, err := load()
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gen == gen {
			if len(c.entries) >= maxAPIKeyCacheEntries {
				clear(c.entries)
			}
			c.entries[keyHash] = apiKeyCacheEntry{tenantID: tenantID, expires: c.now().Add(c.ttl)}
		}
		return tenantID, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (c *apiKeyCache) invalidate(keyHash string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.entries, keyHash)
}

func (c *apiKeyCache) invalidateTenant(tenantID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for hash, e := range c.entries {
		if e.tenantID == tenantID {
			delete(c.entries, hash)
		}
	}
}
