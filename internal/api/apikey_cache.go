package api

import (
	"maps"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// maxAPIKeyCacheEntries bounds the cache, so made-up keys cannot grow it.
const maxAPIKeyCacheEntries = 10_000

// apiKeyCache remembers tenant key lookups; a nil cache remembers nothing.
// How: "Tenant key cache" in docs/architecture.md.
type apiKeyCache struct {
	ttl   time.Duration
	now   func() time.Time
	group singleflight.Group

	mu        sync.Mutex
	byKeyHash map[string]apiKeyCacheEntry // by hashAPIKey of the plaintext key
	// gen changes with every invalidation, so a lookup that read the store
	// before one does not cache what it read.
	gen uint64
}

type apiKeyCacheEntry struct {
	tenantID string // "" = no such key
	expires  time.Time
}

func newAPIKeyCache(ttl time.Duration) *apiKeyCache {
	if ttl <= 0 {
		return nil
	}
	return &apiKeyCache{ttl: ttl, now: time.Now, byKeyHash: make(map[string]apiKeyCacheEntry)}
}

func (c *apiKeyCache) lookup(keyHash string, load func() (string, error)) (string, error) {
	if c == nil {
		return load()
	}
	c.mu.Lock()
	e, ok := c.byKeyHash[keyHash]
	gen := c.gen
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		return e.tenantID, nil
	}

	// Keyed by gen, so a lookup started after a revoke neither waits for nor
	// caches the answer of one started before it.
	v, err, _ := c.group.Do(strconv.FormatUint(gen, 10)+":"+keyHash, func() (any, error) {
		// Expiry counts from before the read, so a slow read cannot keep a key
		// revoked meanwhile on another replica for longer than the TTL.
		started := c.now()
		tenantID, err := load()
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gen == gen {
			if _, refresh := c.byKeyHash[keyHash]; !refresh && len(c.byKeyHash) >= maxAPIKeyCacheEntries {
				c.makeRoom(c.now())
			}
			c.byKeyHash[keyHash] = apiKeyCacheEntry{tenantID: tenantID, expires: started.Add(c.ttl)}
		}
		return tenantID, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// makeRoom drops the expired entries, or all of them when that would free
// less than a quarter, so a flood of made-up keys cannot make every lookup
// scan the whole cache. Called with mu held.
func (c *apiKeyCache) makeRoom(now time.Time) {
	maps.DeleteFunc(c.byKeyHash, func(_ string, e apiKeyCacheEntry) bool {
		return !now.Before(e.expires)
	})
	if len(c.byKeyHash) > maxAPIKeyCacheEntries*3/4 {
		clear(c.byKeyHash)
	}
}

func (c *apiKeyCache) invalidate(keyHash string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.byKeyHash, keyHash)
}

func (c *apiKeyCache) invalidateTenant(tenantID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Even when nothing of the tenant's is cached: a lookup of one of its keys
	// may be reading the store right now.
	c.gen++
	for hash, e := range c.byKeyHash {
		if e.tenantID == tenantID {
			delete(c.byKeyHash, hash)
		}
	}
}
