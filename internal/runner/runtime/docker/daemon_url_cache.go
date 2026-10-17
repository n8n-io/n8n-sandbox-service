package docker

import (
	"sync"
	"time"
)

// daemonURLTombstoneTTL bounds how long a forgotten entry blocks a lookup that
// started before it was forgotten. A lookup is a docker ps and a docker inspect,
// which finish in milliseconds; anything that takes longer than this is a docker
// that has hung, and its result is stored as if the forget never happened.
const daemonURLTombstoneTTL = 2 * time.Minute

// daemonURLCache keeps the daemon URL of running sandboxes, so that a proxied
// request does not have to ask Docker for it. Without it, every request to a
// sandbox runs docker ps and docker inspect, about 20 ms on a fast host.
//
// An entry is trusted only while the death watcher is connected. Every way a
// container stops serving goes through it or through the runner: the runner
// forgets an entry before it stops or removes the container, and the watcher
// forgets it when the container dies on its own. A death while the event stream
// is down is not replayed, so the cache is emptied and not used until the stream
// is back.
//
// A lookup that started before a forget must not store what it read, because the
// container may have stopped since. Each sandbox therefore has a sequence number:
// a forget leaves a tombstone with a new one, and a lookup only stores if the
// number it started with is still current.
type daemonURLCache struct {
	mu sync.Mutex
	// live is true while the death watcher is connected.
	live bool
	// epoch changes when live changes, so a lookup that spans a reconnect does
	// not store a result the watcher may have missed the death of.
	epoch   uint64
	seq     uint64
	entries map[string]daemonURLEntry
}

// daemonURLEntry is a cached URL, or a tombstone when url is empty.
type daemonURLEntry struct {
	containerID string
	url         string
	seq         uint64
	// forgotAt is when a tombstone was left; zero for an entry with a URL.
	forgotAt time.Time
}

// daemonURLToken records the state a lookup started from.
type daemonURLToken struct {
	epoch uint64
	seq   uint64
}

func newDaemonURLCache() *daemonURLCache {
	return &daemonURLCache{entries: make(map[string]daemonURLEntry)}
}

// get returns the cached URL of a sandbox.
func (c *daemonURLCache) get(sandboxID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.live {
		return "", false
	}
	e, ok := c.entries[sandboxID]
	if !ok || e.url == "" {
		return "", false
	}
	return e.url, true
}

// token returns what a lookup passes to store when it is done.
func (c *daemonURLCache) token(sandboxID string) daemonURLToken {
	c.mu.Lock()
	defer c.mu.Unlock()
	return daemonURLToken{epoch: c.epoch, seq: c.entries[sandboxID].seq}
}

// store caches the URL of a container that was ready when the lookup read it,
// unless the sandbox was forgotten or the watcher reconnected since tok.
func (c *daemonURLCache) store(sandboxID, containerID, url string, tok daemonURLToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.live || c.epoch != tok.epoch || c.entries[sandboxID].seq != tok.seq {
		return
	}
	c.entries[sandboxID] = daemonURLEntry{containerID: containerID, url: url, seq: tok.seq}
}

// forget drops the URL of a sandbox and blocks lookups that are in flight.
func (c *daemonURLCache) forget(sandboxID string) {
	if sandboxID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetLocked(sandboxID, time.Now())
}

// forgetContainer drops the URL of the sandbox a container belongs to. It is
// for callers that only have the container ID.
func (c *daemonURLCache) forgetContainer(containerID string) {
	if containerID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, e := range c.entries {
		if e.containerID == containerID {
			c.forgetLocked(id, now)
		}
	}
}

func (c *daemonURLCache) forgetLocked(sandboxID string, now time.Time) {
	// Swept here because only a forget adds tombstones.
	for id, e := range c.entries {
		if e.url == "" && now.Sub(e.forgotAt) > daemonURLTombstoneTTL {
			delete(c.entries, id)
		}
	}
	c.seq++
	c.entries[sandboxID] = daemonURLEntry{seq: c.seq, forgotAt: now}
}

// setLive records whether the death watcher is connected. Both changes empty
// the cache: entries from before a disconnect may name containers that died
// while nothing watched.
func (c *daemonURLCache) setLive(live bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = live
	c.epoch++
	clear(c.entries)
}
