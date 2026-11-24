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

// publishingRow is a row to start a publish of.
func publishingRow(sha, name string) *SharedVolume {
	return &SharedVolume{ID: uuid.New().String(), SHA256: sha, Name: name, PublishedBy: "volume_publisher"}
}

const (
	testMaxLive = 1 << 30
	testLease   = time.Minute
	// lapsedLease starts a publish whose lease has already ended.
	lapsedLease = -time.Minute
)

func startPublish(t *testing.T, s SharedVolumes, row *SharedVolume, lease time.Duration) {
	t.Helper()
	if live, err := s.StartSharedVolumePublish(context.Background(), row, lease, testMaxLive); err != nil || live != nil {
		t.Fatalf("start = %+v, %v; want nil, nil", live, err)
	}
}

func publishReady(t *testing.T, s SharedVolumes, sha, name string) *SharedVolume {
	t.Helper()
	row := publishingRow(sha, name)
	startPublish(t, s, row, testLease)
	row.TarBytes, row.TarEntries, row.TarContentBytes, row.TarDecompressedBytes = 10240, 3, 12, 10240
	if finished, err := s.FinishSharedVolumePublish(context.Background(), row); err != nil || !finished {
		t.Fatalf("finish = %v, %v; want true", finished, err)
	}
	return row
}

func TestSharedVolumes(t *testing.T) {
	testSharedVolumes(t, newSQLiteSharedVolumes(t))
}

func TestSharedVolumesConcurrentStarts(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	testConcurrentSharedVolumeStarts(t, []SharedVolumes{sqlite, sqlite})
}

func TestSharedVolumesConcurrentDeletes(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	testConcurrentSharedVolumeDeletes(t, []SharedVolumes{sqlite, sqlite})
}

func TestSharedVolumeDeleteDuringFinish(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	testDeleteDuringFinish(t, []SharedVolumes{sqlite, sqlite})
}

// A start stops waiting for SQLite's one connection when its context ends.
func TestSQLiteSharedVolumeStartGivesUpWithItsContext(t *testing.T) {
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
		_, err := sqlite.StartSharedVolumePublish(ctx, publishingRow(randomSHA256(), ""), testLease, testMaxLive)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StartSharedVolumePublish() = %v, want the context's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartSharedVolumePublish() kept waiting for the connection")
	}
}

func TestSharedVolumesLimit(t *testing.T) {
	sqlite := newSQLiteSharedVolumes(t)
	ctx := context.Background()
	if _, err := sqlite.StartSharedVolumePublish(ctx, publishingRow(randomSHA256(), ""), testLease, 2); err != nil {
		t.Fatalf("first start: %v", err)
	}
	ready := publishReady(t, sqlite, randomSHA256(), "")
	over := publishingRow(randomSHA256(), "")
	if _, err := sqlite.StartSharedVolumePublish(ctx, over, testLease, 2); !errors.Is(err, ErrSharedVolumeLimit) {
		t.Fatalf("third start = %v, want ErrSharedVolumeLimit", err)
	}
	if got, err := sqlite.GetLiveSharedVolume(ctx, over.SHA256); err != nil || got != nil {
		t.Fatalf("get of the refused start = %+v, %v; want nil: its insert is rolled back", got, err)
	}
	// A deleted row doesn't count.
	if row, err := sqlite.MarkSharedVolumeDeleted(ctx, ready.SHA256); err != nil || row == nil {
		t.Fatalf("delete = %v, %v", row, err)
	}
	if _, err := sqlite.StartSharedVolumePublish(ctx, publishingRow(randomSHA256(), ""), testLease, 2); err != nil {
		t.Fatalf("start after a delete: %v", err)
	}
}

// testSharedVolumes runs against both backends.
func testSharedVolumes(t *testing.T, s SharedVolumes) {
	ctx := context.Background()

	t.Run("start, renew, finish, get and list", func(t *testing.T) {
		sha, name := randomSHA256(), randomVolumeName()
		row := publishingRow(sha, name)
		startPublish(t, s, row, testLease)
		if row.State != SharedVolumePublishing || row.CreatedAt == 0 || row.LeaseUntil != row.CreatedAt+60 {
			t.Fatalf("started row = %+v; want publishing, with a lease a minute from its start", row)
		}
		got, err := s.GetLiveSharedVolume(ctx, sha)
		if err != nil || got == nil || *got != *row {
			t.Fatalf("get publishing = %+v, %v; want %+v", got, err, row)
		}
		if listed, err := s.ListReadySharedVolumes(ctx, name); err != nil || len(listed) != 0 {
			t.Fatalf("list before ready = %v, %v; want none", listed, err)
		}
		if renewed, err := s.RenewSharedVolumePublish(ctx, row.ID, 2*testLease); err != nil || !renewed {
			t.Fatalf("renew = %v, %v; want true", renewed, err)
		}
		row.TarBytes, row.TarEntries, row.TarContentBytes, row.TarDecompressedBytes = 10240, 3, 12, 10240
		row.ImageBytes, row.ImageSHA256 = 4096, randomSHA256()
		if finished, err := s.FinishSharedVolumePublish(ctx, row); err != nil || !finished {
			t.Fatalf("finish = %v, %v; want true", finished, err)
		}
		if row.State != SharedVolumeReady || row.ReadyAt < row.CreatedAt || row.UpdatedAt != row.ReadyAt || row.LeaseUntil != 0 {
			t.Fatalf("finished row = %+v; want ready, at a time the database set", row)
		}
		if got, err = s.GetLiveSharedVolume(ctx, sha); err != nil || got == nil || *got != *row {
			t.Fatalf("get ready = %+v, %v; want %+v", got, err, row)
		}
		if renewed, err := s.RenewSharedVolumePublish(ctx, row.ID, testLease); err != nil || renewed {
			t.Fatalf("renew of a ready row = %v, %v; want false", renewed, err)
		}
		if failed, err := s.FailSharedVolumePublish(ctx, row.ID); err != nil || failed {
			t.Fatalf("fail of a ready row = %v, %v; want false", failed, err)
		}
		listed, err := s.ListReadySharedVolumes(ctx, name)
		if err != nil || len(listed) != 1 || listed[0].ID != row.ID {
			t.Fatalf("list by name = %v, %v; want the row", listed, err)
		}
	})

	t.Run("a live digest or name is refused", func(t *testing.T) {
		sha, name := randomSHA256(), randomVolumeName()
		ready := publishReady(t, s, sha, name)
		live, err := s.StartSharedVolumePublish(ctx, publishingRow(sha, ""), testLease, testMaxLive)
		if !errors.Is(err, ErrSharedVolumeDigestLive) || live == nil || live.ID != ready.ID {
			t.Fatalf("start of a ready digest = %v, %v; want ErrSharedVolumeDigestLive with the row", live, err)
		}
		// Both indexes refuse it; the digest's row answers.
		live, err = s.StartSharedVolumePublish(ctx, publishingRow(sha, name), testLease, testMaxLive)
		if !errors.Is(err, ErrSharedVolumeDigestLive) || live == nil || live.ID != ready.ID {
			t.Fatalf("start of a ready digest under its name = %v, %v; want ErrSharedVolumeDigestLive with the row", live, err)
		}
		if _, err := s.StartSharedVolumePublish(ctx, publishingRow(randomSHA256(), name), testLease, testMaxLive); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("start of a taken name = %v, want ErrSharedVolumeNameTaken", err)
		}

		publishing := publishingRow(randomSHA256(), randomVolumeName())
		startPublish(t, s, publishing, testLease)
		live, err = s.StartSharedVolumePublish(ctx, publishingRow(publishing.SHA256, ""), testLease, testMaxLive)
		if !errors.Is(err, ErrSharedVolumeDigestLive) || live == nil || live.State != SharedVolumePublishing {
			t.Fatalf("start of a publishing digest = %v, %v; want ErrSharedVolumeDigestLive", live, err)
		}
		if _, err := s.StartSharedVolumePublish(ctx, publishingRow(randomSHA256(), publishing.Name), testLease, testMaxLive); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("start of a publishing name = %v, want ErrSharedVolumeNameTaken", err)
		}
	})

	t.Run("a lapsed lease is taken over", func(t *testing.T) {
		name := randomVolumeName()
		lapsed := publishingRow(randomSHA256(), name)
		startPublish(t, s, lapsed, lapsedLease)
		if got, err := s.GetLiveSharedVolume(ctx, lapsed.SHA256); err != nil || got != nil {
			t.Fatalf("get of a lapsed publish = %+v, %v; want nil", got, err)
		}
		startPublish(t, s, publishingRow(lapsed.SHA256, name), testLease)
		if renewed, err := s.RenewSharedVolumePublish(ctx, lapsed.ID, testLease); err != nil || renewed {
			t.Fatalf("renew of the lapsed row = %v, %v; want false", renewed, err)
		}
		if finished, err := s.FinishSharedVolumePublish(ctx, lapsed); err != nil || finished {
			t.Fatalf("finish of the lapsed row = %v, %v; want false", finished, err)
		}
		if failed, err := s.FailSharedVolumePublish(ctx, lapsed.ID); err != nil || !failed {
			t.Fatalf("fail of the lapsed row = %v, %v; want true: it is failed", failed, err)
		}

		// Another digest under a lapsed publish's name.
		byName := publishingRow(randomSHA256(), randomVolumeName())
		startPublish(t, s, byName, lapsedLease)
		startPublish(t, s, publishingRow(randomSHA256(), byName.Name), testLease)
		if renewed, err := s.RenewSharedVolumePublish(ctx, byName.ID, testLease); err != nil || renewed {
			t.Fatalf("renew of the row whose name was taken over = %v, %v; want false", renewed, err)
		}
	})

	t.Run("naming a ready volume", func(t *testing.T) {
		unnamed := publishReady(t, s, randomSHA256(), "")
		other := publishReady(t, s, randomSHA256(), randomVolumeName())
		if _, err := s.NameReadySharedVolume(ctx, unnamed.ID, other.Name); !errors.Is(err, ErrSharedVolumeNameTaken) {
			t.Fatalf("name with a taken name = %v, want ErrSharedVolumeNameTaken", err)
		}
		name := randomVolumeName()
		if named, err := s.NameReadySharedVolume(ctx, unnamed.ID, name); err != nil || !named {
			t.Fatalf("name = %v, %v; want true", named, err)
		}
		if named, err := s.NameReadySharedVolume(ctx, unnamed.ID, randomVolumeName()); err != nil || named {
			t.Fatalf("second name = %v, %v; want false: one name per volume", named, err)
		}
		if got, _ := s.GetLiveSharedVolume(ctx, unnamed.SHA256); got == nil || got.Name != name {
			t.Fatalf("get after naming = %+v", got)
		}
	})

	t.Run("delete hides a ready volume and frees its name", func(t *testing.T) {
		name := randomVolumeName()
		ready := publishReady(t, s, randomSHA256(), name)
		deleted, err := s.MarkSharedVolumeDeleted(ctx, ready.SHA256)
		if err != nil || deleted == nil || deleted.State != SharedVolumeDeleted || deleted.ID != ready.ID {
			t.Fatalf("delete = %+v, %v; want the row, deleted", deleted, err)
		}
		if got, err := s.GetLiveSharedVolume(ctx, ready.SHA256); err != nil || got != nil {
			t.Fatalf("get after delete = %+v, %v; want nil", got, err)
		}
		if again, err := s.MarkSharedVolumeDeleted(ctx, ready.SHA256); err != nil || again != nil {
			t.Fatalf("second delete = %+v, %v; want nil", again, err)
		}
		startPublish(t, s, publishingRow(randomSHA256(), name), testLease)
		startPublish(t, s, publishingRow(ready.SHA256, ""), testLease)
	})

	t.Run("delete leaves a publishing volume, and fails a lapsed one", func(t *testing.T) {
		publishing := publishingRow(randomSHA256(), "")
		startPublish(t, s, publishing, testLease)
		got, err := s.MarkSharedVolumeDeleted(ctx, publishing.SHA256)
		if err != nil || got == nil || got.State != SharedVolumePublishing {
			t.Fatalf("delete while publishing = %+v, %v; want the publishing row", got, err)
		}
		lapsed := publishingRow(randomSHA256(), "")
		startPublish(t, s, lapsed, lapsedLease)
		if got, err := s.MarkSharedVolumeDeleted(ctx, lapsed.SHA256); err != nil || got != nil {
			t.Fatalf("delete after the lease ended = %+v, %v; want nil", got, err)
		}
		if failed, err := s.FailSharedVolumePublish(ctx, lapsed.ID); err != nil || !failed {
			t.Fatalf("the lapsed row is failed = %v, %v; want true", failed, err)
		}
	})

	t.Run("purge takes failed and deleted rows only", func(t *testing.T) {
		ready := publishReady(t, s, randomSHA256(), "")
		if purged, err := s.PurgeSharedVolume(ctx, ready.ID); err != nil || purged {
			t.Fatalf("purge of a ready row = %v, %v; want false", purged, err)
		}
		if _, err := s.MarkSharedVolumeDeleted(ctx, ready.SHA256); err != nil {
			t.Fatal(err)
		}
		if purged, err := s.PurgeSharedVolume(ctx, ready.ID); err != nil || !purged {
			t.Fatalf("purge of a deleted row = %v, %v; want true", purged, err)
		}
		if purged, err := s.PurgeSharedVolume(ctx, ready.ID); err != nil || purged {
			t.Fatalf("second purge = %v, %v; want false", purged, err)
		}
		publishing := publishingRow(randomSHA256(), "")
		startPublish(t, s, publishing, testLease)
		if purged, err := s.PurgeSharedVolume(ctx, publishing.ID); err != nil || purged {
			t.Fatalf("purge of a publishing row = %v, %v; want false", purged, err)
		}
		if failed, err := s.FailSharedVolumePublish(ctx, publishing.ID); err != nil || !failed {
			t.Fatalf("fail = %v, %v; want true", failed, err)
		}
		if purged, err := s.PurgeSharedVolume(ctx, publishing.ID); err != nil || !purged {
			t.Fatalf("purge of a failed row = %v, %v; want true", purged, err)
		}
	})

	t.Run("list is newest first", func(t *testing.T) {
		older := publishReady(t, s, randomSHA256(), "")
		// ready_at counts whole seconds of the database clock.
		time.Sleep(1100 * time.Millisecond)
		newer := publishReady(t, s, randomSHA256(), "")
		listed, err := s.ListReadySharedVolumes(ctx, "")
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

// testConcurrentSharedVolumeStarts starts publishing one digest from every
// store at once: exactly one wins, the others see its row.
func testConcurrentSharedVolumeStarts(t *testing.T, stores []SharedVolumes) {
	ctx := context.Background()
	sha := randomSHA256()
	const perStore = 4
	var (
		wait   sync.WaitGroup
		mutex  sync.Mutex
		won    int
		others int
	)
	// Holds every start back until all of them are running.
	start := make(chan struct{})
	for _, s := range stores {
		for range perStore {
			wait.Go(func() {
				<-start
				_, err := s.StartSharedVolumePublish(ctx, publishingRow(sha, ""), testLease, testMaxLive)
				mutex.Lock()
				defer mutex.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, ErrSharedVolumeDigestLive):
					others++
				default:
					t.Errorf("start: %v", err)
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
	publishReady(t, stores[0], sha, "")
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
				row, err := records.MarkSharedVolumeDeleted(ctx, sha)
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

// testDeleteDuringFinish deletes volumes whose publish finishes at the same
// time: each delete gets the volume, deleted or still publishing, never
// nothing.
func testDeleteDuringFinish(t *testing.T, stores []SharedVolumes) {
	ctx := context.Background()
	for range 200 {
		row := publishingRow(randomSHA256(), "")
		startPublish(t, stores[0], row, testLease)
		sha := row.SHA256
		var (
			wait      sync.WaitGroup
			found     *SharedVolume
			deleteErr error
		)
		start := make(chan struct{})
		wait.Go(func() {
			<-start
			if _, err := stores[0].FinishSharedVolumePublish(ctx, row); err != nil {
				t.Errorf("finish: %v", err)
			}
		})
		wait.Go(func() {
			<-start
			found, deleteErr = stores[1].MarkSharedVolumeDeleted(ctx, sha)
		})
		close(start)
		wait.Wait()
		if deleteErr != nil {
			t.Fatalf("delete: %v", deleteErr)
		}
		if found == nil {
			live, _ := stores[0].GetLiveSharedVolume(ctx, sha)
			t.Fatalf("delete during the finish found no volume; the volume is %+v", live)
		}
	}
}
