package api

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedCount returns a count func reporting *saved for every tenant.
func fixedCount(saved *atomic.Int64) func(string) (int64, error) {
	return func(string) (int64, error) { return saved.Load(), nil }
}

func TestTenantQuotaCountsCreatesInProgress(t *testing.T) {
	var saved atomic.Int64
	saved.Store(1)
	q := newTenantQuota(fixedCount(&saved))

	var releases []func()
	for i := range 2 {
		release, ok, err := q.reserve("a", 3)
		if err != nil || !ok {
			t.Fatalf("reserve %d: ok=%v err=%v, want a slot below the limit", i, ok, err)
		}
		releases = append(releases, release)
	}
	if _, ok, err := q.reserve("a", 3); err != nil || ok {
		t.Fatalf("reserve at 1 saved + 2 in progress: ok=%v err=%v, want refused at limit 3", ok, err)
	}
	if release, ok, err := q.reserve("b", 3); err != nil || !ok {
		t.Fatalf("reserve for another tenant: ok=%v err=%v, want a slot", ok, err)
	} else {
		release()
	}

	releases[0]()
	release, ok, err := q.reserve("a", 3)
	if err != nil || !ok {
		t.Fatalf("reserve after a release: ok=%v err=%v, want the freed slot", ok, err)
	}
	release()
	releases[1]()
}

// A create saved and released while another reserve reads the saved count must
// still be counted, though it is in neither the read nor the creates in
// progress. Here the read returns what was saved before that create's save,
// which finishes, release included, before the read returns.
func TestTenantQuotaCountsCreateSavedDuringCountRead(t *testing.T) {
	var saved atomic.Int64
	var releaseFirst func()
	q := newTenantQuota(func(string) (int64, error) {
		before := saved.Load()
		if releaseFirst != nil {
			saved.Add(1)
			releaseFirst()
		}
		return before, nil
	})

	var ok bool
	var err error
	releaseFirst, ok, err = q.reserve("a", 1)
	if err != nil || !ok {
		t.Fatalf("first reserve: ok=%v err=%v", ok, err)
	}
	if _, ok, err := q.reserve("a", 1); err != nil || ok {
		t.Fatalf("second reserve: ok=%v err=%v, want refused: the first create is saved", ok, err)
	}
	if len(q.tenants) != 0 {
		t.Fatalf("tenants = %v, want empty", q.tenants)
	}
}

// A create saved before the read and released during it is in the read and
// counted as finished during it. That must not refuse a create below the
// limit: the reserve reads again.
func TestTenantQuotaRereadsWhenCreateReleasedDuringRead(t *testing.T) {
	var saved, calls atomic.Int64
	var releaseFirst func()
	q := newTenantQuota(func(string) (int64, error) {
		if calls.Add(1) == 2 {
			saved.Add(1)
			releaseFirst()
		}
		return saved.Load(), nil
	})

	var ok bool
	var err error
	releaseFirst, ok, err = q.reserve("a", 2)
	if err != nil || !ok {
		t.Fatalf("first reserve: ok=%v err=%v", ok, err)
	}
	release, ok, err := q.reserve("a", 2)
	if err != nil || !ok {
		t.Fatalf("second reserve with 1 saved at limit 2: ok=%v err=%v, want a slot", ok, err)
	}
	release()
	if n := calls.Load(); n != 3 {
		t.Fatalf("count reads = %d, want 3: one for the first reserve, two for the second", n)
	}
}

// A tenant's entry must outlive every reserve using it. If it were dropped
// when its last create is released while another reserve is reading, a new
// create would start a fresh entry, and the reading reserve would no longer
// see that create in progress.
func TestTenantQuotaKeepsEntryWhileReserveReads(t *testing.T) {
	var saved, calls atomic.Int64
	var releaseFirst func()
	var q *tenantQuota
	q = newTenantQuota(func(string) (int64, error) {
		before := saved.Load()
		if calls.Add(1) == 2 {
			saved.Add(1)
			releaseFirst()
			if _, ok, err := q.reserve("a", 2); err != nil || !ok {
				t.Errorf("reserve during the read: ok=%v err=%v, want a slot", ok, err)
			}
			return before, nil
		}
		return saved.Load(), nil
	})

	var ok bool
	var err error
	releaseFirst, ok, err = q.reserve("a", 2)
	if err != nil || !ok {
		t.Fatalf("first reserve: ok=%v err=%v", ok, err)
	}
	if _, ok, err := q.reserve("a", 2); err != nil || ok {
		t.Fatalf("reserve at 1 saved + 1 in progress: ok=%v err=%v, want refused at limit 2", ok, err)
	}
}

// The saved count is read without holding up the tenant's other creates: a
// slow read for one create must not delay another's reserve or release.
func TestTenantQuotaSlowCountReadBlocksNoOtherCreate(t *testing.T) {
	inRead := make(chan struct{})
	finishRead := make(chan struct{})
	finish := sync.OnceFunc(func() { close(finishRead) })
	defer finish()
	var calls atomic.Int32
	q := newTenantQuota(func(string) (int64, error) {
		if calls.Add(1) == 1 {
			close(inRead)
			<-finishRead
		}
		return 0, nil
	})

	slow := make(chan bool)
	go func() {
		release, ok, _ := q.reserve("a", 5)
		if ok {
			release()
		}
		slow <- ok
	}()
	<-inRead

	done := make(chan struct{})
	go func() {
		defer close(done)
		release, ok, err := q.reserve("a", 5)
		if err != nil || !ok {
			t.Errorf("reserve during a slow read: ok=%v err=%v", ok, err)
			return
		}
		release()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reserve and release waited for another create's count read")
	}
	finish()
	if !<-slow {
		t.Fatal("slow reserve refused, want a slot")
	}
}

// The limit has to hold under contention: every admitted create is saved, so
// a burst leaves exactly the limit saved.
func TestTenantQuotaNeverAdmitsPastLimitConcurrently(t *testing.T) {
	const limit = 5
	var saved atomic.Int64
	q := newTenantQuota(fixedCount(&saved))
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			release, ok, err := q.reserve("a", limit)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if !ok {
				return
			}
			time.Sleep(time.Millisecond)
			saved.Add(1)
			release()
		})
	}
	wg.Wait()
	if n := saved.Load(); n != limit {
		t.Fatalf("saved = %d, want exactly the limit %d", n, limit)
	}
	if len(q.tenants) != 0 {
		t.Fatalf("tenants after all released = %v, want empty", q.tenants)
	}
}

func TestTenantQuotaUnlimitedCountsNothing(t *testing.T) {
	q := newTenantQuota(func(string) (int64, error) {
		t.Fatal("count read for an unlimited tenant")
		return 0, nil
	})
	for range 3 {
		release, ok, err := q.reserve("a", 0)
		if err != nil || !ok {
			t.Fatalf("reserve unlimited: ok=%v err=%v", ok, err)
		}
		release()
	}
	if len(q.tenants) != 0 {
		t.Fatalf("tenants = %v, want none tracked when unlimited", q.tenants)
	}
}

func TestTenantQuotaReturnsCountError(t *testing.T) {
	boom := errors.New("store down")
	q := newTenantQuota(func(string) (int64, error) { return 0, boom })
	if _, ok, err := q.reserve("a", 1); !errors.Is(err, boom) || ok {
		t.Fatalf("reserve: ok=%v err=%v, want the count error", ok, err)
	}
	if len(q.tenants) != 0 {
		t.Fatalf("tenants = %v, want none left after a failed reserve", q.tenants)
	}
}

// Tenant IDs come and go; entries must not outlive the creates using them.
func TestTenantQuotaForgetsTenantsWithNothingInProgress(t *testing.T) {
	var saved atomic.Int64
	q := newTenantQuota(fixedCount(&saved))
	releaseA, _, _ := q.reserve("a", 2)
	releaseB, _, _ := q.reserve("b", 2)
	if _, ok, _ := q.reserve("b", 1); ok {
		t.Fatal("reserve for b at limit 1: admitted, want refused")
	}
	releaseB()
	if _, ok := q.tenants["b"]; ok {
		t.Error("tenant b is still tracked with nothing in progress")
	}
	if e := q.tenants["a"]; e == nil || e.inProgress != 1 {
		t.Fatalf("tenant a = %+v, want 1 in progress", e)
	}
	releaseA()
	if len(q.tenants) != 0 {
		t.Fatalf("tenants = %v, want empty", q.tenants)
	}
}

func TestTenantQuotaReleaseTwiceFreesOneSlot(t *testing.T) {
	var saved atomic.Int64
	q := newTenantQuota(fixedCount(&saved))
	release, _, _ := q.reserve("a", 3)
	other, _, _ := q.reserve("a", 3)
	release()
	release()
	if e := q.tenants["a"]; e == nil || e.inProgress != 1 {
		t.Fatalf("tenant a = %+v, want 1 in progress", e)
	}
	other()
}
