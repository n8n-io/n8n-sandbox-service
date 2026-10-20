package api

import "sync"

// tenantQuota checks max_sandboxes before a create reaches a runner. Besides a
// tenant's saved sandboxes it counts the creates on this API pod that passed
// the check and are not saved yet, so a burst on one pod cannot pass the check
// together. Creates on other pods count once they are saved; until then only
// the store's check when saving stops them.
type tenantQuota struct {
	count func(tenantID string) (int64, error)

	mu      sync.Mutex
	tenants map[string]*tenantCreates
}

// tenantCreates is one tenant's creates on this pod, guarded by tenantQuota.mu.
type tenantCreates struct {
	inProgress int
	saved      int64 // creates saved so far; only differences are used
	reading    int   // reserves reading the saved count
}

// newTenantQuota returns a tenantQuota that reads a tenant's saved sandboxes
// with count.
func newTenantQuota(count func(tenantID string) (int64, error)) *tenantQuota {
	return &tenantQuota{count: count, tenants: make(map[string]*tenantCreates)}
}

// reserve takes a slot for one create when the tenant's saved sandboxes plus
// its creates in progress on this pod are below limit. A limit of 0 or less is
// unlimited and counts nothing. ok is false when the quota is full.
//
// Call release(true) right after the sandbox is saved, so it is not counted
// both as saved and in progress for long, or when it may have been saved.
// Call release(false) once a failed create is gone from its runner. Calls
// after the first do nothing.
func (q *tenantQuota) reserve(tenantID string, limit int) (release func(saved bool), ok bool, err error) {
	if limit <= 0 {
		return func(bool) {}, true, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.tenants[tenantID]
	if e == nil {
		e = &tenantCreates{}
		q.tenants[tenantID] = e
	}
	e.reading++
	defer func() {
		e.reading--
		q.forgetIfIdle(tenantID, e)
	}()

	const maxReads = 3
	for read := 1; ; read++ {
		savedBefore := e.saved
		n, err := q.countUnlocked(tenantID)
		if err != nil {
			return nil, false, err
		}
		// A create saved during the read is no longer in inProgress, and may
		// not be in n if it was saved after the read started: count it too.
		savedDuring := e.saved - savedBefore
		if n+int64(e.inProgress)+savedDuring < int64(limit) {
			e.inProgress++
			var once sync.Once
			return func(saved bool) {
				once.Do(func() {
					q.mu.Lock()
					defer q.mu.Unlock()
					e.inProgress--
					if saved {
						e.saved++
					}
					q.forgetIfIdle(tenantID, e)
				})
			}, true, nil
		}
		// Those creates may be in n as well, and refuse a create below the
		// limit. A new read sees them, unless creates keep being saved during
		// every read.
		if savedDuring == 0 || read == maxReads {
			return nil, false, nil
		}
	}
}

// countUnlocked reads the tenant's saved count with q.mu released, so a slow
// read holds up no other create. Call with q.mu held.
func (q *tenantQuota) countUnlocked(tenantID string) (int64, error) {
	q.mu.Unlock()
	defer q.mu.Lock()
	return q.count(tenantID)
}

// forgetIfIdle drops e once no create is in progress and no reserve is reading,
// so saved never restarts under a reading reserve. Call with q.mu held.
func (q *tenantQuota) forgetIfIdle(tenantID string, e *tenantCreates) {
	if e.inProgress == 0 && e.reading == 0 {
		delete(q.tenants, tenantID)
	}
}
