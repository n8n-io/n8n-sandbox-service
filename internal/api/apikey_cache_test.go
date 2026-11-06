package api

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

func newTestAPIKeyCache(t *testing.T, ttl time.Duration) (*apiKeyCache, func(time.Duration)) {
	t.Helper()
	c := newAPIKeyCache(ttl)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	return c, func(d time.Duration) { now = now.Add(d) }
}

// countingLoad returns a load function answering tenantID and the number of
// times it ran.
func countingLoad(tenantID string, err error) (func() (string, error), *int) {
	n := 0
	return func() (string, error) {
		n++
		return tenantID, err
	}, &n
}

func TestAPIKeyCacheRemembersKnownAndUnknownKeysUntilTheyExpire(t *testing.T) {
	c, advance := newTestAPIKeyCache(t, 15*time.Second)
	for _, tenantID := range []string{"tenant-a", ""} {
		load, loads := countingLoad(tenantID, nil)
		hash := "hash-" + tenantID
		for range 3 {
			got, err := c.lookup(hash, load)
			if err != nil || got != tenantID {
				t.Fatalf("lookup(%q) = %q, %v, want %q", hash, got, err, tenantID)
			}
		}
		if *loads != 1 {
			t.Fatalf("lookup(%q) loaded %d times, want 1", hash, *loads)
		}
	}

	advance(15 * time.Second)
	load, loads := countingLoad("tenant-a", nil)
	if _, err := c.lookup("hash-tenant-a", load); err != nil || *loads != 1 {
		t.Fatalf("after expiry: loads = %d err=%v, want a fresh load", *loads, err)
	}
}

// A slow read must not keep a key that another replica revoked meanwhile for
// longer than the TTL.
func TestAPIKeyCacheExpiryCountsFromBeforeTheRead(t *testing.T) {
	c, advance := newTestAPIKeyCache(t, time.Minute)
	if _, err := c.lookup("hash", func() (string, error) {
		advance(30 * time.Second)
		return "tenant-a", nil
	}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	advance(30 * time.Second)
	load, loads := countingLoad("", nil)
	if got, _ := c.lookup("hash", load); got != "" || *loads != 1 {
		t.Fatalf("one TTL after the read started: %q loads=%d, want a fresh load", got, *loads)
	}
}

func TestAPIKeyCacheDoesNotCacheErrors(t *testing.T) {
	c, _ := newTestAPIKeyCache(t, time.Minute)
	failing, _ := countingLoad("", errors.New("database unavailable"))
	if _, err := c.lookup("hash", failing); err == nil {
		t.Fatal("lookup returned no error from a failing load")
	}
	load, loads := countingLoad("tenant-a", nil)
	if got, err := c.lookup("hash", load); err != nil || got != "tenant-a" || *loads != 1 {
		t.Fatalf("after a failed load: %q err=%v loads=%d, want tenant-a from a fresh load", got, err, *loads)
	}
}

func TestAPIKeyCacheOffLoadsEveryTime(t *testing.T) {
	c := newAPIKeyCache(0)
	load, loads := countingLoad("tenant-a", nil)
	for range 3 {
		if _, err := c.lookup("hash", load); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	if *loads != 3 {
		t.Fatalf("loads = %d, want 3 with the cache off", *loads)
	}
	c.invalidate("hash")
	c.invalidateTenant("tenant-a")
}

func TestAPIKeyCacheInvalidation(t *testing.T) {
	c, _ := newTestAPIKeyCache(t, time.Minute)
	for _, k := range []struct{ hash, tenantID string }{
		{"a1", "tenant-a"}, {"a2", "tenant-a"}, {"b1", "tenant-b"},
	} {
		load, _ := countingLoad(k.tenantID, nil)
		if _, err := c.lookup(k.hash, load); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	cached := func(hash string) bool {
		load, loads := countingLoad("reloaded", nil)
		_, _ = c.lookup(hash, load)
		return *loads == 0
	}

	c.invalidateTenant("tenant-a")
	if cached("a1") || cached("a2") {
		t.Fatal("tenant-a's keys survived invalidateTenant")
	}
	if !cached("b1") {
		t.Fatal("tenant-b's key was dropped by tenant-a's invalidation")
	}
	c.invalidate("b1")
	if cached("b1") {
		t.Fatal("b1 survived invalidate")
	}
}

// A lookup that read the store before a revoke or tenant delete must not cache
// what it read, even when nothing was cached for the key or tenant yet.
func TestAPIKeyCacheIgnoresLoadsOverlappingAnInvalidation(t *testing.T) {
	for name, invalidate := range map[string]func(*apiKeyCache){
		"revoke":        func(c *apiKeyCache) { c.invalidate("hash") },
		"tenant delete": func(c *apiKeyCache) { c.invalidateTenant("tenant-a") },
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestAPIKeyCache(t, time.Minute)
			if _, err := c.lookup("hash", func() (string, error) {
				invalidate(c)
				return "tenant-a", nil
			}); err != nil {
				t.Fatalf("lookup: %v", err)
			}
			load, loads := countingLoad("", nil)
			if got, _ := c.lookup("hash", load); got != "" || *loads != 1 {
				t.Fatalf("after an overlapping %s: %q loads=%d, want a fresh load", name, got, *loads)
			}
		})
	}
}

// A full cache drops its expired entries, or starts over when that frees less
// than a quarter, so made-up keys can neither grow it nor make every lookup
// scan it.
func TestAPIKeyCacheMakesRoomWhenFull(t *testing.T) {
	for _, tc := range []struct {
		name        string
		expired     int
		wantKnown   bool
		wantEntries int
	}{
		{"drops the expired entries", maxAPIKeyCacheEntries / 4, true, maxAPIKeyCacheEntries*3/4 + 1},
		{"starts over when that frees less than a quarter", maxAPIKeyCacheEntries/4 - 1, false, 1},
		{"starts over when nothing has expired", 0, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, advance := newTestAPIKeyCache(t, time.Minute)
			unknown, _ := countingLoad("", nil)
			fill := func(prefix string, n int) {
				t.Helper()
				for i := range n {
					if _, err := c.lookup(fmt.Sprintf("%s-%d", prefix, i), unknown); err != nil {
						t.Fatalf("lookup: %v", err)
					}
				}
			}
			fill("expired", tc.expired)
			advance(time.Minute)
			known, _ := countingLoad("tenant-a", nil)
			if _, err := c.lookup("known", known); err != nil {
				t.Fatalf("lookup: %v", err)
			}
			fill("live", maxAPIKeyCacheEntries-tc.expired-1)
			if n := len(c.byKeyHash); n != maxAPIKeyCacheEntries {
				t.Fatalf("entries = %d, want %d", n, maxAPIKeyCacheEntries)
			}

			_, _ = c.lookup("one-more", unknown)
			if n := len(c.byKeyHash); n != tc.wantEntries {
				t.Fatalf("entries after one more = %d, want %d", n, tc.wantEntries)
			}
			again, loads := countingLoad("tenant-a", nil)
			if got, _ := c.lookup("known", again); got != "tenant-a" || (*loads == 0) != tc.wantKnown {
				t.Fatalf("known key after making room: %q loads=%d, want it kept: %v", got, *loads, tc.wantKnown)
			}
		})
	}
}

// Reloading an expired key in a full cache replaces its entry and drops no
// other one.
func TestAPIKeyCacheRefreshInAFullCacheKeepsTheOthers(t *testing.T) {
	c, advance := newTestAPIKeyCache(t, time.Minute)
	known, _ := countingLoad("tenant-a", nil)
	if _, err := c.lookup("known", known); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	advance(time.Minute)
	unknown, _ := countingLoad("", nil)
	for i := range maxAPIKeyCacheEntries - 1 {
		if _, err := c.lookup(fmt.Sprintf("live-%d", i), unknown); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}

	again, loads := countingLoad("tenant-a", nil)
	if got, _ := c.lookup("known", again); got != "tenant-a" || *loads != 1 {
		t.Fatalf("expired key: %q loads=%d, want tenant-a from a fresh load", got, *loads)
	}
	if n := len(c.byKeyHash); n != maxAPIKeyCacheEntries {
		t.Fatalf("entries after the refresh = %d, want %d", n, maxAPIKeyCacheEntries)
	}
	live, liveLoads := countingLoad("", nil)
	_, _ = c.lookup("live-0", live)
	if *liveLoads != 0 {
		t.Fatal("a live entry was dropped by the refresh")
	}
}

// A lookup that starts after a revoke must not wait for, and share the answer
// of, one that started before it.
func TestAPIKeyCacheLookupAfterInvalidationDoesNotJoinAnEarlierOne(t *testing.T) {
	c, _ := newTestAPIKeyCache(t, time.Minute)
	entered, release := make(chan struct{}), make(chan struct{})
	earlier := make(chan string, 1)
	go func() {
		tenantID, _ := c.lookup("hash", func() (string, error) {
			close(entered)
			<-release
			return "tenant-a", nil
		})
		earlier <- tenantID
	}()
	<-entered

	c.invalidate("hash")
	revoked, loads := countingLoad("", nil)
	if got, err := c.lookup("hash", revoked); err != nil || got != "" || *loads != 1 {
		t.Fatalf("lookup after the revoke = %q err=%v loads=%d, want its own load answering unknown", got, err, *loads)
	}

	close(release)
	if got := <-earlier; got != "tenant-a" {
		t.Fatalf("earlier lookup = %q, want tenant-a", got)
	}
	again, againLoads := countingLoad("", nil)
	if got, _ := c.lookup("hash", again); got != "" || *againLoads != 0 {
		t.Fatalf("after both: %q loads=%d, want the cached unknown answer", got, *againLoads)
	}
}

// keyLookupCountingStore counts tenant key lookups that reach the store.
type keyLookupCountingStore struct {
	store.SandboxStore
	lookups atomic.Int64
}

func (s *keyLookupCountingStore) ListActiveAPIKeysByPrefix(prefix string) ([]*store.APIKey, error) {
	s.lookups.Add(1)
	return s.SandboxStore.ListActiveAPIKeysByPrefix(prefix)
}

// newCachingTestGateways returns one router per API pod over one store, each
// with its own key cache.
func newCachingTestGateways(t *testing.T, pods int) ([]http.Handler, *keyLookupCountingStore) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cs := &keyLookupCountingStore{SandboxStore: s}

	cfg := withRunnerTLS(&config.APIConfig{
		APIKeys:             map[string]struct{}{"admin-key": {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: 50,
		APIKeyCacheTTL:      time.Minute,
	})
	routers := make([]http.Handler, pods)
	for i := range routers {
		routers[i], err = NewGatewayRouter(cs, cfg, registry.New(45*time.Second), metrics.NewAPIRecorder(false), nil)
		if err != nil {
			t.Fatalf("create gateway router: %v", err)
		}
	}
	return routers, cs
}

func TestTenantKeyLookupsAreCached(t *testing.T) {
	routers, s := newCachingTestGateways(t, 1)
	router := routers[0]
	created := mintTenantKey(t, router, `{"name":"acme"}`)

	for range 3 {
		if rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
			t.Fatalf("tenant list: %d %s", rr.Code, rr.Body.String())
		}
	}
	if n := s.lookups.Load(); n != 1 {
		t.Fatalf("store lookups for one key = %d, want 1", n)
	}

	const wrong = "sbk_00000000_000000000000000000000000000000000000000000000000"
	for range 3 {
		if rr := doJSON(t, router, http.MethodGet, "/sandboxes", wrong, ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong key: %d, want 401", rr.Code)
		}
	}
	if n := s.lookups.Load(); n != 2 {
		t.Fatalf("store lookups after a repeated wrong key = %d, want 2", n)
	}
}

// The pod that revokes a key refuses it at once. Other pods accept it until
// their cached lookup expires, which docs/API.md states.
func TestRevokedTenantKeyWithCache(t *testing.T) {
	routers, _ := newCachingTestGateways(t, 2)
	podA, podB := routers[0], routers[1]
	created := mintTenantKey(t, podA, `{"name":"acme"}`)
	for _, pod := range routers {
		if rr := doJSON(t, pod, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
			t.Fatalf("tenant list before revoke: %d", rr.Code)
		}
	}

	path := "/admin/tenants/" + created.Tenant.ID + "/keys/" + created.Key.ID
	if rr := doJSON(t, podA, http.MethodDelete, path, "admin-key", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, podA, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key on the revoking pod: %d, want 401", rr.Code)
	}
	if rr := doJSON(t, podB, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoked key on another pod within the TTL: %d, want 200 from its cache", rr.Code)
	}
}

func TestDeletedTenantKeyRejectedAtOnceWithCache(t *testing.T) {
	routers, _ := newCachingTestGateways(t, 1)
	router := routers[0]
	created := mintTenantKey(t, router, `{"name":"acme"}`)
	if rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
		t.Fatalf("tenant list before delete: %d", rr.Code)
	}

	if rr := doJSON(t, router, http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, "admin-key", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete tenant: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted tenant's key: %d, want 401", rr.Code)
	}
}

// Deleting a tenant again on a replica that still remembers its key, after
// another replica deleted it, clears the key there too.
func TestRepeatedTenantDeleteClearsTheCacheOnAnotherPod(t *testing.T) {
	routers, _ := newCachingTestGateways(t, 2)
	podA, podB := routers[0], routers[1]
	created := mintTenantKey(t, podA, `{"name":"acme"}`)
	if rr := doJSON(t, podB, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
		t.Fatalf("tenant list before delete: %d", rr.Code)
	}

	path := "/admin/tenants/" + created.Tenant.ID
	if rr := doJSON(t, podA, http.MethodDelete, path, "admin-key", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete tenant: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, podB, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
		t.Fatalf("deleted tenant's key on another pod within the TTL: %d, want 200 from its cache", rr.Code)
	}
	if rr := doJSON(t, podB, http.MethodDelete, path, "admin-key", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete tenant again: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, podB, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted tenant's key after deleting it again on that pod: %d, want 401", rr.Code)
	}
}

// revokeErrStore revokes, then reports an error, as when a commit's reply is
// lost.
type revokeErrStore struct{ *keyLookupCountingStore }

func (s revokeErrStore) RevokeAPIKey(id string) error {
	if err := s.keyLookupCountingStore.RevokeAPIKey(id); err != nil {
		return err
	}
	return errors.New("connection reset after commit")
}

func TestRevokeClearsTheCacheEvenWhenTheStoreReportsAnError(t *testing.T) {
	routers, cs := newCachingTestGateways(t, 1)
	created := mintTenantKey(t, routers[0], `{"name":"acme"}`)
	router, err := NewGatewayRouter(revokeErrStore{cs}, withRunnerTLS(&config.APIConfig{
		APIKeys:        map[string]struct{}{"admin-key": {}},
		RunnerAPIKey:   "runner-key",
		MaxFileBytes:   1024,
		APIKeyCacheTTL: time.Minute,
	}), registry.New(45*time.Second), metrics.NewAPIRecorder(false), nil)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	if rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusOK {
		t.Fatalf("tenant list before revoke: %d", rr.Code)
	}

	path := "/admin/tenants/" + created.Tenant.ID + "/keys/" + created.Key.ID
	if rr := doJSON(t, router, http.MethodDelete, path, "admin-key", ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("revoke: %d, want 500 from the store error", rr.Code)
	}
	if rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("key revoked despite the error: %d, want 401", rr.Code)
	}
}
