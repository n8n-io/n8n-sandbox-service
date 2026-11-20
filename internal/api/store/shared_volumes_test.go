package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newSQLiteSharedVolumes(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// randomSHA256 keeps rows of tests that share a Postgres database apart.
func randomSHA256() string {
	sum := sha256.Sum256([]byte(uuid.New().String()))
	return hex.EncodeToString(sum[:])
}

func randomVolumeName() string {
	return "volume-" + uuid.New().String()[:8]
}

// publishingRow is a row to reserve, with a lease that ends at leaseUntil.
func publishingRow(sha, name string, now, leaseUntil int64) *SharedVolume {
	return &SharedVolume{
		ID:          uuid.New().String(),
		SHA256:      sha,
		Name:        name,
		State:       SharedVolumePublishing,
		PublishedBy: "publisher",
		CreatedAt:   now,
		UpdatedAt:   now,
		LeaseUntil:  leaseUntil,
	}
}

const testMaxLive = 1 << 30

func reserveReady(t *testing.T, s SharedVolumes, sha, name string, readyAt int64) *SharedVolume {
	t.Helper()
	ctx := context.Background()
	row := publishingRow(sha, name, readyAt, readyAt+120)
	if _, err := s.ReserveSharedVolume(ctx, row, testMaxLive); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	row.TarBytes, row.Entries, row.ContentBytes, row.DecompressedBytes = 10240, 3, 12, 10240
	row.ReadyAt = readyAt
	if finished, err := s.FinishSharedVolume(ctx, row); err != nil || !finished {
		t.Fatalf("finish = %v, %v; want true", finished, err)
	}
	return row
}

func TestSharedVolumes(t *testing.T) {
	testSharedVolumes(t, newSQLiteSharedVolumes(t))
}

func TestSharedVolumesConcurrentReserves(t *testing.T) {
	s := newSQLiteSharedVolumes(t)
	testConcurrentSharedVolumeReserves(t, []SharedVolumes{s, s})
}

func TestSharedVolumesConcurrentDeletes(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	testConcurrentSharedVolumeDeletes(t, []SharedVolumes{sqlite, sqlite})
}

// A reservation stops waiting for SQLite's one connection when its context
// ends.
func TestSQLiteSharedVolumeReserveGivesUpWithItsContext(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	held, err := sqlite.beginImmediate()
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackConn(held)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sqlite.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), "", 100, 220), testMaxLive)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReserveSharedVolume() = %v, want the context's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReserveSharedVolume() kept waiting for the connection")
	}
}

func TestSharedVolumesLimit(t *testing.T) {
	s := newSQLiteSharedVolumes(t)
	ctx := context.Background()
	if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), "", 100, 200), 2); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	ready := reserveReady(t, s, randomSHA256(), "", 100)
	if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), "", 100, 200), 2); !errors.Is(err, ErrSharedVolumeLimit) {
		t.Fatalf("third reserve = %v, want ErrSharedVolumeLimit", err)
	}
	// A deleted row doesn't count.
	if row, err := s.DeleteSharedVolume(ctx, ready.SHA256, 101); err != nil || row == nil {
		t.Fatalf("delete = %v, %v", row, err)
	}
	if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), "", 102, 300), 2); err != nil {
		t.Fatalf("reserve after a delete: %v", err)
	}
}

// testSharedVolumes runs against both backends.
func testSharedVolumes(t *testing.T, s SharedVolumes) {
	ctx := context.Background()

	t.Run("reserve, renew, finish, get and list", func(t *testing.T) {
		sha, name := randomSHA256(), randomVolumeName()
		row := publishingRow(sha, name, 100, 220)
		if live, err := s.ReserveSharedVolume(ctx, row, testMaxLive); err != nil || live != nil {
			t.Fatalf("reserve = %v, %v; want nil, nil", live, err)
		}
		got, err := s.GetSharedVolume(ctx, sha)
		if err != nil || got == nil || got.State != SharedVolumePublishing || got.ID != row.ID || got.Name != name {
			t.Fatalf("get publishing = %+v, %v", got, err)
		}
		if listed, err := s.ListSharedVolumes(ctx, name); err != nil || len(listed) != 0 {
			t.Fatalf("list before ready = %v, %v; want none", listed, err)
		}
		if kept, err := s.RenewSharedVolumeLease(ctx, row.ID, 400); err != nil || !kept {
			t.Fatalf("renew = %v, %v; want true", kept, err)
		}
		row.TarBytes, row.Entries, row.ContentBytes, row.DecompressedBytes = 10240, 3, 12, 10240
		row.ImageBytes, row.ImageSHA256, row.ReadyAt = 4096, randomSHA256(), 150
		if finished, err := s.FinishSharedVolume(ctx, row); err != nil || !finished {
			t.Fatalf("finish = %v, %v; want true", finished, err)
		}
		got, err = s.GetSharedVolume(ctx, sha)
		if err != nil || got == nil {
			t.Fatalf("get ready = %v, %v", got, err)
		}
		want := *row
		want.State, want.UpdatedAt, want.LeaseUntil = SharedVolumeReady, 150, 0
		if *got != want {
			t.Fatalf("get ready = %+v, want %+v", *got, want)
		}
		if kept, err := s.RenewSharedVolumeLease(ctx, row.ID, 500); err != nil || kept {
			t.Fatalf("renew of a ready row = %v, %v; want false", kept, err)
		}
		if failed, err := s.FailSharedVolume(ctx, row.ID, 160); err != nil || failed {
			t.Fatalf("fail of a ready row = %v, %v; want false", failed, err)
		}
		listed, err := s.ListSharedVolumes(ctx, name)
		if err != nil || len(listed) != 1 || listed[0].ID != row.ID {
			t.Fatalf("list by name = %v, %v; want the row", listed, err)
		}
	})

	t.Run("a live digest or name is refused", func(t *testing.T) {
		sha, name := randomSHA256(), randomVolumeName()
		ready := reserveReady(t, s, sha, name, 100)
		live, err := s.ReserveSharedVolume(ctx, publishingRow(sha, "", 110, 230), testMaxLive)
		if !errors.Is(err, ErrSharedVolumeDigestLive) || live == nil || live.ID != ready.ID {
			t.Fatalf("reserve of a ready digest = %v, %v; want ErrSharedVolumeDigestLive with the row", live, err)
		}
		if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), name, 110, 230), testMaxLive); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("reserve of a taken name = %v, want ErrSharedVolumeNameTaken", err)
		}

		publishing := publishingRow(randomSHA256(), randomVolumeName(), 100, 220)
		if _, err := s.ReserveSharedVolume(ctx, publishing, testMaxLive); err != nil {
			t.Fatal(err)
		}
		live, err = s.ReserveSharedVolume(ctx, publishingRow(publishing.SHA256, "", 110, 230), testMaxLive)
		if !errors.Is(err, ErrSharedVolumeDigestLive) || live == nil || live.State != SharedVolumePublishing {
			t.Fatalf("reserve of a publishing digest = %v, %v; want ErrSharedVolumeDigestLive", live, err)
		}
		if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), publishing.Name, 110, 230), testMaxLive); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("reserve of a publishing name = %v, want ErrSharedVolumeNameTaken", err)
		}
	})

	t.Run("a lapsed lease is taken over", func(t *testing.T) {
		name := randomVolumeName()
		lapsed := publishingRow(randomSHA256(), name, 100, 220)
		if _, err := s.ReserveSharedVolume(ctx, lapsed, testMaxLive); err != nil {
			t.Fatal(err)
		}
		// The same digest at 230, after the lease ended.
		retry := publishingRow(lapsed.SHA256, name, 230, 350)
		if _, err := s.ReserveSharedVolume(ctx, retry, testMaxLive); err != nil {
			t.Fatalf("reserve after the lease ended: %v", err)
		}
		if kept, err := s.RenewSharedVolumeLease(ctx, lapsed.ID, 400); err != nil || kept {
			t.Fatalf("renew of the lapsed row = %v, %v; want false", kept, err)
		}
		if finished, err := s.FinishSharedVolume(ctx, lapsed); err != nil || finished {
			t.Fatalf("finish of the lapsed row = %v, %v; want false", finished, err)
		}
		if failed, err := s.FailSharedVolume(ctx, lapsed.ID, 240); err != nil || !failed {
			t.Fatalf("fail of the lapsed row = %v, %v; want true: it is failed", failed, err)
		}

		// Another digest under the lapsed name at 400.
		byName := publishingRow(randomSHA256(), randomVolumeName(), 100, 220)
		if _, err := s.ReserveSharedVolume(ctx, byName, testMaxLive); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), byName.Name, 400, 520), testMaxLive); err != nil {
			t.Fatalf("reserve of a lapsed name: %v", err)
		}
		if kept, err := s.RenewSharedVolumeLease(ctx, byName.ID, 600); err != nil || kept {
			t.Fatalf("renew of the row whose name was taken over = %v, %v; want false", kept, err)
		}
	})

	t.Run("naming a ready volume", func(t *testing.T) {
		unnamed := reserveReady(t, s, randomSHA256(), "", 100)
		other := reserveReady(t, s, randomSHA256(), randomVolumeName(), 100)
		if _, err := s.NameSharedVolume(ctx, unnamed.ID, other.Name); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("name with a taken name = %v, want ErrSharedVolumeNameTaken", err)
		}
		name := randomVolumeName()
		if named, err := s.NameSharedVolume(ctx, unnamed.ID, name); err != nil || !named {
			t.Fatalf("name = %v, %v; want true", named, err)
		}
		if named, err := s.NameSharedVolume(ctx, unnamed.ID, randomVolumeName()); err != nil || named {
			t.Fatalf("second name = %v, %v; want false: one name per volume", named, err)
		}
		if got, _ := s.GetSharedVolume(ctx, unnamed.SHA256); got == nil || got.Name != name {
			t.Fatalf("get after naming = %+v", got)
		}
	})

	t.Run("delete hides a ready volume and frees its name", func(t *testing.T) {
		name := randomVolumeName()
		ready := reserveReady(t, s, randomSHA256(), name, 100)
		deleted, err := s.DeleteSharedVolume(ctx, ready.SHA256, 200)
		if err != nil || deleted == nil || deleted.State != SharedVolumeDeleted || deleted.ID != ready.ID {
			t.Fatalf("delete = %+v, %v; want the row, deleted", deleted, err)
		}
		if got, err := s.GetSharedVolume(ctx, ready.SHA256); err != nil || got != nil {
			t.Fatalf("get after delete = %+v, %v; want nil", got, err)
		}
		if again, err := s.DeleteSharedVolume(ctx, ready.SHA256, 210); err != nil || again != nil {
			t.Fatalf("second delete = %+v, %v; want nil", again, err)
		}
		if _, err := s.ReserveSharedVolume(ctx, publishingRow(randomSHA256(), name, 220, 340), testMaxLive); err != nil {
			t.Fatalf("reserve of the freed name: %v", err)
		}
		if _, err := s.ReserveSharedVolume(ctx, publishingRow(ready.SHA256, "", 220, 340), testMaxLive); err != nil {
			t.Fatalf("reserve of the deleted digest: %v", err)
		}
	})

	t.Run("delete leaves a publishing volume, and fails a lapsed one", func(t *testing.T) {
		publishing := publishingRow(randomSHA256(), "", 100, 220)
		if _, err := s.ReserveSharedVolume(ctx, publishing, testMaxLive); err != nil {
			t.Fatal(err)
		}
		got, err := s.DeleteSharedVolume(ctx, publishing.SHA256, 200)
		if err != nil || got == nil || got.State != SharedVolumePublishing {
			t.Fatalf("delete while publishing = %+v, %v; want the publishing row", got, err)
		}
		if got, err := s.DeleteSharedVolume(ctx, publishing.SHA256, 230); err != nil || got != nil {
			t.Fatalf("delete after the lease ended = %+v, %v; want nil", got, err)
		}
		if failed, err := s.FailSharedVolume(ctx, publishing.ID, 240); err != nil || !failed {
			t.Fatalf("the lapsed row is failed = %v, %v; want true", failed, err)
		}
	})

	t.Run("remove takes every row but a ready one", func(t *testing.T) {
		ready := reserveReady(t, s, randomSHA256(), "", 100)
		if removed, err := s.RemoveSharedVolume(ctx, ready.ID); err != nil || removed {
			t.Fatalf("remove of a ready row = %v, %v; want false", removed, err)
		}
		if _, err := s.DeleteSharedVolume(ctx, ready.SHA256, 200); err != nil {
			t.Fatal(err)
		}
		if removed, err := s.RemoveSharedVolume(ctx, ready.ID); err != nil || !removed {
			t.Fatalf("remove of a deleted row = %v, %v; want true", removed, err)
		}
		if removed, err := s.RemoveSharedVolume(ctx, ready.ID); err != nil || removed {
			t.Fatalf("second remove = %v, %v; want false", removed, err)
		}
		publishing := publishingRow(randomSHA256(), "", 100, 220)
		if _, err := s.ReserveSharedVolume(ctx, publishing, testMaxLive); err != nil {
			t.Fatal(err)
		}
		if failed, err := s.FailSharedVolume(ctx, publishing.ID, 150); err != nil || !failed {
			t.Fatalf("fail = %v, %v; want true", failed, err)
		}
		if removed, err := s.RemoveSharedVolume(ctx, publishing.ID); err != nil || !removed {
			t.Fatalf("remove of a failed row = %v, %v; want true", removed, err)
		}
	})

	t.Run("list is newest first", func(t *testing.T) {
		now := time.Now().Unix()
		older := reserveReady(t, s, randomSHA256(), "", now)
		newer := reserveReady(t, s, randomSHA256(), "", now+1)
		listed, err := s.ListSharedVolumes(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		position := map[string]int{}
		for index, row := range listed {
			position[row.ID] = index
			if index > 0 && row.ReadyAt > listed[index-1].ReadyAt {
				t.Fatalf("row %d is newer than the one before it", index)
			}
		}
		olderAt, olderFound := position[older.ID]
		newerAt, newerFound := position[newer.ID]
		if !olderFound || !newerFound || newerAt > olderAt {
			t.Fatalf("list has the newer row at %d and the older at %d; want both, newer first", newerAt, olderAt)
		}
	})
}

// testConcurrentSharedVolumeReserves reserves one digest from every store at
// once: exactly one wins, the others see its row.
func testConcurrentSharedVolumeReserves(t *testing.T, stores []SharedVolumes) {
	ctx := context.Background()
	sha := randomSHA256()
	const perStore = 4
	var (
		wait   sync.WaitGroup
		mutex  sync.Mutex
		won    int
		others int
	)
	// Holds every reservation back until all of them are started.
	start := make(chan struct{})
	for _, s := range stores {
		for range perStore {
			wait.Go(func() {
				<-start
				_, err := s.ReserveSharedVolume(ctx, publishingRow(sha, "", 100, 220), testMaxLive)
				mutex.Lock()
				defer mutex.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, ErrSharedVolumeDigestLive):
					others++
				default:
					t.Errorf("reserve: %v", err)
				}
			})
		}
	}
	close(start)
	wait.Wait()
	if won != 1 || others != len(stores)*perStore-1 {
		t.Fatalf("won %d, refused %d; want 1 and the rest", won, others)
	}
}

// testConcurrentSharedVolumeDeletes deletes one ready volume from every store
// at once: exactly one delete gets the row, the others find none.
func testConcurrentSharedVolumeDeletes(t *testing.T, stores []SharedVolumes) {
	ctx := context.Background()
	sha := randomSHA256()
	reserveReady(t, stores[0], sha, "", 100)
	const perStore = 4
	var (
		wait    sync.WaitGroup
		mutex   sync.Mutex
		deleted int
	)
	start := make(chan struct{})
	for _, records := range stores {
		for range perStore {
			wait.Go(func() {
				<-start
				row, err := records.DeleteSharedVolume(ctx, sha, 200)
				mutex.Lock()
				defer mutex.Unlock()
				switch {
				case err != nil:
					t.Errorf("delete: %v", err)
				case row == nil:
				case row.State == SharedVolumeDeleted:
					deleted++
				default:
					t.Errorf("delete returned a %s row", row.State)
				}
			})
		}
	}
	close(start)
	wait.Wait()
	if deleted != 1 {
		t.Fatalf("%d deletes got the row, want 1", deleted)
	}
}
