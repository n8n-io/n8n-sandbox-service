package volumes

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

func TestPublishStoresTheTarWithItsCounts(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)

	published, err := env.publish(t, data, "n8n-assistant-2.45.5")
	if err != nil {
		t.Fatalf("Publish() = %v", err)
	}
	if !published.Created {
		t.Error("Created = false, want true")
	}
	stats, err := safetar.Validate(bytes.NewReader(data), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	row := published.Volume
	want := volume.TarCounts{Entries: int64(stats.Entries), ContentBytes: stats.Bytes, DecompressedBytes: stats.DecompressedBytes}
	got := volume.TarCounts{Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes}
	if got != want || row.TarBytes != int64(len(data)) || row.State != store.SharedVolumeReady || row.Name != "n8n-assistant-2.45.5" {
		t.Fatalf("row = %+v, want counts %+v, %d bytes, ready and named", row, want, len(data))
	}
	// docs and docs/guide are left out of the tar and counted.
	if row.TarEntries != 6 {
		t.Errorf("TarEntries = %d, want 6", row.TarEntries)
	}
	object, err := env.local.Stat(context.Background(), objectKey(row.ID, volume.KindTar))
	if err != nil || object.SHA256 != digestOf(data) || object.Size != int64(len(data)) {
		t.Fatalf("stored object = %+v, %v; want the tar", object, err)
	}
	if got, err := env.service.Get(context.Background(), digestOf(data)); err != nil || got.ID != row.ID {
		t.Fatalf("Get() = %+v, %v", got, err)
	}
	if count := env.metrics.PublishCount(metrics.SharedVolumeCreated); count != 1 {
		t.Errorf("created publishes = %v, want 1", count)
	}
}

func TestPublishTakesGzip(t *testing.T) {
	env := newTestEnv(t)
	data := gzipped(t, validTar(t))
	published, err := env.publish(t, data, "")
	if err != nil {
		t.Fatalf("Publish() = %v", err)
	}
	if published.Volume.TarBytes != int64(len(data)) || published.Volume.TarDecompressedBytes <= int64(len(data)) {
		t.Fatalf("row = %+v, want the gzip's size and a larger decompressed size", published.Volume)
	}
}

func TestRepeatedPublishDoesNotReadTheBody(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	first, err := env.publish(t, data, "")
	if err != nil {
		t.Fatal(err)
	}
	repeat := func(name string) (PublishResult, error) {
		return env.service.Publish(context.Background(), PublishRequest{
			SHA256: digestOf(data), Name: name, Size: int64(len(data)), Body: unreadable{t}, Role: RoleAdmin,
		})
	}

	again, err := repeat("")
	if err != nil || again.Created || again.Volume.ID != first.Volume.ID {
		t.Fatalf("repeat = %+v, %v; want the same volume, not created", again, err)
	}
	named, err := repeat("first-name")
	if err != nil || named.Volume.Name != "first-name" {
		t.Fatalf("repeat with a name = %+v, %v; want it named", named, err)
	}
	if _, err := repeat("first-name"); err != nil {
		t.Fatalf("repeat with its name = %v", err)
	}
	if _, err := repeat("second-name"); !errors.Is(err, ErrNameMismatch) {
		t.Fatalf("repeat with another name = %v, want ErrNameMismatch", err)
	}
	if count := env.metrics.PublishCount(metrics.SharedVolumeExists); count != 3 {
		t.Errorf("exists publishes = %v, want 3", count)
	}
}

func TestPublishRefusesATakenNameBeforeReadingTheBody(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.publish(t, validTar(t), "taken"); err != nil {
		t.Fatal(err)
	}
	other := makeTar(t, file("other", "x"))
	_, err := env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(other), Name: "taken", Size: int64(len(other)), Body: unreadable{t}, Role: RoleVolumePublisher,
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("Publish() = %v, want ErrNameTaken", err)
	}
	if len(env.records.started) != 1 {
		t.Fatalf("started %v, want only the first volume", env.records.started)
	}
	if files := env.storedFiles(t); len(files) != 1 {
		t.Fatalf("store holds %v, want the first tar only", files)
	}
}

func TestPublishLeavesNothingOfARefusedUpload(t *testing.T) {
	tooManyEntries := make([]tarEntry, 17)
	for index := range tooManyEntries {
		tooManyEntries[index] = file(strings.Repeat("f", index+1), "")
	}
	valid := validTar(t)
	for name, tc := range map[string]struct {
		data   []byte
		digest string
		size   int64
		want   error
	}{
		"wrong digest":                  {data: valid, digest: digestOf([]byte("other")), want: ErrDigestMismatch},
		"hard link":                     {data: makeTar(t, file("a", "x"), tarEntry{name: "b", typeflag: '1', linkname: "a"}), want: ErrInvalidTar},
		"device":                        {data: makeTar(t, tarEntry{name: "dev", typeflag: '3'}), want: ErrInvalidTar},
		"dot dot":                       {data: makeTar(t, file("../escape", "x")), want: ErrInvalidTar},
		"absolute path":                 {data: makeTar(t, file("/etc/passwd", "x")), want: ErrInvalidTar},
		"through a link":                {data: makeTar(t, link("dir", "/etc"), file("dir/passwd", "x")), want: ErrInvalidTar},
		"no entries":                    {data: makeTar(t), want: ErrInvalidTar},
		"not a tar":                     {data: bytes.Repeat([]byte("junk"), 300), want: ErrInvalidTar},
		"cut-off gzip":                  {data: cutInHalf(gzipped(t, valid)), want: ErrInvalidTar},
		"too many entries":              {data: makeTar(t, tooManyEntries...), want: ErrTooLarge},
		"too much content":              {data: makeTar(t, file("big", strings.Repeat("x", 1<<14+1))), want: ErrTooLarge},
		"too large decompressed":        {data: gzipped(t, makeTar(t, file("a", strings.Repeat("x", 1<<14)), file("b", strings.Repeat("x", 1<<14)), file("c", strings.Repeat("x", 1<<14)), file("d", strings.Repeat("x", 1<<14)))), want: ErrTooLarge},
		"Content-Length over the limit": {data: valid, size: 1<<20 + 1, want: ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			if name == "too large decompressed" {
				env.service.limits.policy = volume.TarPolicy(16, 1<<20, 1<<16)
			}
			digest, size := tc.digest, tc.size
			if digest == "" {
				digest = digestOf(tc.data)
			}
			if size == 0 {
				size = int64(len(tc.data))
			}
			_, err := env.service.Publish(context.Background(), PublishRequest{
				SHA256: digest, Size: size, Body: bytes.NewReader(tc.data), Role: RoleVolumePublisher,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Publish() = %v, want %v", err, tc.want)
			}
			env.assertNothingLeft(t)
			if _, err := env.service.Get(context.Background(), digest); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get() after a refused publish = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestRefusedTarErrorsNameNoEntry(t *testing.T) {
	env := newTestEnv(t)
	data := makeTar(t, file("secret-entry-name/../x", "x"))
	_, err := env.publish(t, data, "")
	if !errors.Is(err, ErrInvalidTar) || strings.Contains(err.Error(), "secret-entry-name") {
		t.Fatalf("Publish() = %v; want ErrInvalidTar naming no entry", err)
	}
	big := makeTar(t, file("secret-big-file", strings.Repeat("x", 1<<14+1)))
	if _, err := env.publish(t, big, ""); !errors.Is(err, ErrTooLarge) || strings.Contains(err.Error(), "secret-big-file") {
		t.Fatalf("Publish() = %v; want ErrTooLarge naming no entry", err)
	}
}

func TestPublishOfAnEmptyBody(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.service.Publish(context.Background(), PublishRequest{SHA256: digestOf(nil), Body: unreadable{t}, Role: RoleAdmin})
	if !errors.Is(err, ErrInvalidTar) {
		t.Fatalf("Publish() = %v, want ErrInvalidTar", err)
	}
}

func TestPublishOfAClientThatGoesAway(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	body := &chunkedReader{data: data, piece: 512, failAt: 1024}
	_, err := env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(data), Size: int64(len(data)), Body: body, Role: RoleVolumePublisher,
	})
	if !errors.Is(err, ErrUpload) {
		t.Fatalf("Publish() = %v, want ErrUpload", err)
	}
	env.assertNothingLeft(t)
	if count := env.metrics.PublishCount(metrics.SharedVolumeInterrupted); count != 1 {
		t.Errorf("interrupted publishes = %v, want 1", count)
	}
}

func TestPublishOfAClientWhoseRequestEnds(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	next := make(chan struct{}, 1)
	next <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := env.service.Publish(ctx, PublishRequest{
			SHA256: digestOf(data), Size: int64(len(data)), Body: &chunkedReader{data: data, piece: 512, next: next}, Role: RoleVolumePublisher,
		})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(next)
	if err := <-done; !errors.Is(err, ErrUpload) {
		t.Fatalf("Publish() = %v, want ErrUpload", err)
	}
	env.assertNothingLeft(t)
}

func TestPublishOfAStoreThatFails(t *testing.T) {
	env := newTestEnv(t)
	env.objects.failPut = true
	if _, err := env.publish(t, validTar(t), "name"); err == nil || errors.Is(err, ErrInvalidTar) {
		t.Fatalf("Publish() = %v, want a store error", err)
	}
	env.assertNothingLeft(t)
	if count := env.metrics.PublishCount(metrics.SharedVolumeError); count != 1 {
		t.Errorf("error publishes = %v, want 1", count)
	}
	// The name is free again.
	env.objects.failPut = false
	if _, err := env.publish(t, makeTar(t, file("x", "y")), "name"); err != nil {
		t.Fatalf("Publish() of the freed name = %v", err)
	}
}

func TestFailedCleanupLeavesTheRowFailed(t *testing.T) {
	env := newTestEnv(t)
	env.objects.failDelete = true
	data := validTar(t)
	_, err := env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf([]byte("other")), Size: int64(len(data)), Body: bytes.NewReader(data), Role: RoleVolumePublisher,
	})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Publish() = %v, want ErrDigestMismatch", err)
	}
	if len(env.records.purged) != 0 {
		t.Fatalf("purged %v, want the row kept for a later purge", env.records.purged)
	}
	// Failed, so it no longer holds the digest.
	env.objects.failDelete = false
	if _, err := env.publish(t, data, ""); err != nil {
		t.Fatalf("Publish() after the failed one = %v", err)
	}
}

// A finish whose reply got lost made the row ready: the cleanup must leave its
// objects alone.
func TestLostFinishReplyKeepsTheReadyVolume(t *testing.T) {
	env := newTestEnv(t)
	env.records.finishLosesReply = true
	data := validTar(t)
	if _, err := env.publish(t, data, ""); err == nil {
		t.Fatal("Publish() = nil, want the lost reply's error")
	}
	row, err := env.service.Get(context.Background(), digestOf(data))
	if err != nil {
		t.Fatalf("Get() = %v; want the volume ready", err)
	}
	if _, err := env.local.Stat(context.Background(), objectKey(row.ID, volume.KindTar)); err != nil {
		t.Fatalf("the ready volume's tar = %v; want it kept", err)
	}
}

func TestPublishInProgressAndThePublishLimit(t *testing.T) {
	env := newTestEnv(t)
	first, second := validTar(t), makeTar(t, file("second", "x"))
	start := func(data []byte, next chan struct{}) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := env.service.Publish(context.Background(), PublishRequest{
				SHA256: digestOf(data), Size: int64(len(data)), Body: &chunkedReader{data: data, piece: 512, next: next}, Role: RoleVolumePublisher,
			})
			done <- err
		}()
		return done
	}
	firstNext, secondNext := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := start(first, firstNext), start(second, secondNext)
	waitFor(t, func() bool {
		firstRow, _ := env.records.GetLiveSharedVolume(context.Background(), digestOf(first))
		secondRow, _ := env.records.GetLiveSharedVolume(context.Background(), digestOf(second))
		return firstRow != nil && secondRow != nil
	})

	if _, err := env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(first), Size: int64(len(first)), Body: unreadable{t}, Role: RoleVolumePublisher,
	}); !errors.Is(err, ErrPublishInProgress) {
		t.Fatalf("Publish() of a digest being published = %v, want ErrPublishInProgress", err)
	}
	third := makeTar(t, file("third", "x"))
	if _, err := env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(third), Size: int64(len(third)), Body: unreadable{t}, Role: RoleVolumePublisher,
	}); !errors.Is(err, ErrPublishLimit) {
		t.Fatalf("Publish() over the limit = %v, want ErrPublishLimit", err)
	}
	if err := env.service.Delete(context.Background(), digestOf(first), RoleVolumePublisher); !errors.Is(err, ErrPublishInProgress) {
		t.Fatalf("Delete() of a digest being published = %v, want ErrPublishInProgress", err)
	}

	close(firstNext)
	close(secondNext)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Publish() = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Publish() = %v", err)
	}
}

func TestShutdownInterruptsAPublish(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	next := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := env.service.Publish(context.Background(), PublishRequest{
			SHA256: digestOf(data), Size: int64(len(data)), Body: &chunkedReader{data: data, piece: 512, next: next}, Role: RoleVolumePublisher,
		})
		done <- err
	}()
	waitFor(t, func() bool { return len(env.service.runningPublishes) == 1 })
	env.shutdown()
	close(next)
	if err := <-done; !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
	env.assertNothingLeft(t)
}

func TestALostLeaseInterruptsAPublish(t *testing.T) {
	env := newTestEnv(t)
	env.service.timing.renewEvery = 10 * time.Millisecond
	data := validTar(t)
	next := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := env.service.Publish(context.Background(), PublishRequest{
			SHA256: digestOf(data), Size: int64(len(data)), Body: &chunkedReader{data: data, piece: 512, next: next}, Role: RoleVolumePublisher,
		})
		done <- err
	}()
	var row *store.SharedVolume
	waitFor(t, func() bool {
		row, _ = env.records.GetLiveSharedVolume(context.Background(), digestOf(data))
		return row != nil
	})
	// Another pod took the row over.
	if failed, err := env.records.FailSharedVolumePublish(context.Background(), row.ID); err != nil || !failed {
		t.Fatalf("fail the row = %v, %v", failed, err)
	}
	time.Sleep(50 * time.Millisecond)
	close(next)
	if err := <-done; !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
	env.assertNothingLeft(t)
}

// startWaitingPublish publishes from a body that waits for the client until
// the publish interrupts it.
func startWaitingPublish(env *testEnv, data []byte) chan error {
	stop := make(chan struct{})
	var stopOnce sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := env.service.Publish(context.Background(), PublishRequest{
			SHA256: digestOf(data), Size: int64(len(data)), Role: RoleVolumePublisher,
			Body:      &chunkedReader{data: data, piece: 512, next: make(chan struct{}), stop: stop},
			Interrupt: func() { stopOnce.Do(func() { close(stop) }) },
		})
		done <- err
	}()
	return done
}

// publishError waits for a publish to return.
func publishError(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Publish() did not return")
		return nil
	}
}

func TestShutdownInterruptsAWaitingUpload(t *testing.T) {
	env := newTestEnv(t)
	done := startWaitingPublish(env, validTar(t))
	waitFor(t, func() bool { return len(env.service.runningPublishes) == 1 })
	env.shutdown()
	if err := publishError(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
	env.assertNothingLeft(t)
}

func TestAHangingRenewalEndsThePublish(t *testing.T) {
	env := newTestEnv(t)
	env.records.renewHangs = true
	env.service.timing.renewEvery = 10 * time.Millisecond
	env.service.timing.giveUp = 100 * time.Millisecond
	done := startWaitingPublish(env, validTar(t))
	if err := publishError(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
	env.assertNothingLeft(t)
}

// A publish whose upload is done does not wait for a renewal that hangs.
func TestAHangingRenewalDoesNotHoldUpAPublish(t *testing.T) {
	env := newTestEnv(t)
	env.records.renewHangs = true
	env.service.timing.renewEvery = 10 * time.Millisecond
	data := validTar(t)
	next := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := env.service.Publish(context.Background(), PublishRequest{
			SHA256: digestOf(data), Size: int64(len(data)), Role: RoleVolumePublisher,
			Body: &chunkedReader{data: data, piece: 512, next: next},
		})
		done <- err
	}()
	waitFor(t, func() bool { return env.records.renewalCount() > 0 })
	close(next)
	if err := publishError(t, done); err != nil {
		t.Fatalf("Publish() = %v", err)
	}
}

func TestALapsedLeaseIsTakenOver(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	// A publish on a pod that died: its row is publishing with a lease that ended.
	lapsed := &store.SharedVolume{ID: "lapsed", SHA256: digestOf(data), Name: "name", PublishedBy: string(RoleVolumePublisher)}
	if _, err := env.records.SharedVolumes.StartSharedVolumePublish(context.Background(), lapsed, -time.Minute, 100); err != nil {
		t.Fatal(err)
	}
	published, err := env.publish(t, data, "name")
	if err != nil || !published.Created || published.Volume.ID == "lapsed" {
		t.Fatalf("Publish() = %+v, %v; want a new volume", published, err)
	}
}

func TestDeleteRemovesTheVolume(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	published, err := env.publish(t, data, "name")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.service.Delete(context.Background(), digestOf(data), RoleVolumePublisher); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := env.service.Get(context.Background(), digestOf(data)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after Delete() = %v, want ErrNotFound", err)
	}
	if err := env.service.Delete(context.Background(), digestOf(data), RoleVolumePublisher); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete() = %v, want ErrNotFound", err)
	}
	env.assertNothingLeft(t)
	// The name and the digest can be published again, under a new row.
	again, err := env.publish(t, data, "name")
	if err != nil || again.Volume.ID == published.Volume.ID {
		t.Fatalf("Publish() after Delete() = %+v, %v; want a new volume", again, err)
	}
}

func TestDeleteWhoseRemovalFailsHidesTheVolume(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	if _, err := env.publish(t, data, "name"); err != nil {
		t.Fatal(err)
	}
	env.objects.failDelete = true
	if err := env.service.Delete(context.Background(), digestOf(data), RoleAdmin); err != nil {
		t.Fatalf("Delete() = %v; the removal is left for later", err)
	}
	if _, err := env.service.Get(context.Background(), digestOf(data)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() = %v, want ErrNotFound", err)
	}
	if listed, _ := env.service.List(context.Background(), "name"); len(listed) != 0 {
		t.Fatalf("List() = %v, want nothing", listed)
	}
}

func TestListAndItsNameFilter(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.publish(t, validTar(t), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.publish(t, makeTar(t, file("x", "y")), ""); err != nil {
		t.Fatal(err)
	}
	all, err := env.service.List(context.Background(), "")
	if err != nil || len(all) != 2 {
		t.Fatalf("List() = %v, %v; want 2", all, err)
	}
	named, err := env.service.List(context.Background(), "first")
	if err != nil || len(named) != 1 || named[0].Name != "first" {
		t.Fatalf("List(first) = %v, %v", named, err)
	}
	if _, err := env.service.List(context.Background(), "Not A Name"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("List() with an invalid name = %v, want ErrInvalidName", err)
	}
}

func TestPublishThroughARemoteStore(t *testing.T) {
	helper, remote := newMemoryHelper(t)
	env := newTestEnvWith(t, remote, t.TempDir())
	data := validTar(t)

	for name, tc := range map[string]struct {
		data   []byte
		digest string
		want   error
	}{
		"wrong digest": {data: data, digest: digestOf([]byte("x")), want: ErrDigestMismatch},
		"hard link":    {data: makeTar(t, file("a", "x"), tarEntry{name: "b", typeflag: '1', linkname: "a"}), want: ErrInvalidTar},
		"no entries":   {data: makeTar(t), want: ErrInvalidTar},
		"too large":    {data: makeTar(t, file("big", strings.Repeat("x", 1<<14+1))), want: ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			digest := tc.digest
			if digest == "" {
				digest = digestOf(tc.data)
			}
			_, err := env.service.Publish(context.Background(), PublishRequest{
				SHA256: digest, Size: int64(len(tc.data)), Body: bytes.NewReader(tc.data), Role: RoleVolumePublisher,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Publish() = %v, want %v", err, tc.want)
			}
			if count := helper.count(); count != 0 {
				t.Fatalf("the helper stored %d objects, want none", count)
			}
		})
	}
	env.assertNothingLeft(t)

	published, err := env.publish(t, data, "")
	if err != nil || !published.Created {
		t.Fatalf("Publish() = %+v, %v", published, err)
	}
	object, err := remote.Stat(context.Background(), objectKey(published.Volume.ID, volume.KindTar))
	if err != nil || object.SHA256 != digestOf(data) {
		t.Fatalf("stored object = %+v, %v", object, err)
	}
	if err := env.service.Delete(context.Background(), digestOf(data), RoleVolumePublisher); err != nil {
		t.Fatal(err)
	}
	if count := helper.count(); count != 0 {
		t.Fatalf("the helper holds %d objects after Delete(), want none", count)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"n8n-assistant-2.45.5":        true,
		"n8n-assistant-2.46.0-beta.1": true,
		"a":                           true,
		"a_b":                         true,
		strings.Repeat("a", 128):      true,
		strings.Repeat("a", 129):      false,
		"":                            false,
		"-a":                          false,
		"a-":                          false,
		"A":                           false,
		"sha256:abc":                  false,
		"a/b":                         false,
		"a b":                         false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func cutInHalf(data []byte) []byte {
	return data[:len(data)/2]
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var _ volumestore.Store = (*failingObjects)(nil)
