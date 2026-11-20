package volumes

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// setClock makes env's service see the time at.
func (env *testEnv) setClock(at time.Time) {
	env.service.now = func() time.Time { return at }
}

// stateOf returns the state of the row id, or "" when it is gone. Failed and
// deleted rows are among the removable ones; the others are live.
func (env *testEnv) stateOf(t *testing.T, id, sha256 string) store.SharedVolumeState {
	t.Helper()
	rows, err := env.records.ListRemovableSharedVolumes(context.Background(), math.MaxInt64, store.SharedVolumePosition{}, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row.State
		}
	}
	live, err := env.records.GetSharedVolume(context.Background(), sha256)
	if err != nil {
		t.Fatal(err)
	}
	if live != nil && live.ID == id {
		return live.State
	}
	return ""
}

func (env *testEnv) hasObject(t *testing.T, id string, kind volume.Kind) bool {
	t.Helper()
	_, err := env.local.Stat(context.Background(), objectKey(id, kind))
	if err != nil && !errors.Is(err, volumestore.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestSweepFailsALapsedPublishAndRemovesItAfterTheGrace(t *testing.T) {
	env := newTestEnv(t)
	start := time.Now()
	// A publish on a pod that died after it stored the tar.
	data := validTar(t)
	lapsed := &store.SharedVolume{
		ID: "lapsed", SHA256: digestOf(data), Name: "name", State: store.SharedVolumePublishing,
		PublishedBy: "publisher", CreatedAt: start.Unix() - 600, UpdatedAt: start.Unix() - 600, LeaseUntil: start.Unix() - 300,
	}
	// One whose lease ended just now, which another instance's clock may not
	// see yet.
	other := makeTar(t, file("other", "x"))
	justEnded := &store.SharedVolume{
		ID: "just-ended", SHA256: digestOf(other), State: store.SharedVolumePublishing,
		PublishedBy: "publisher", CreatedAt: start.Unix() - 600, UpdatedAt: start.Unix() - 600, LeaseUntil: start.Unix() - 1,
	}
	for _, row := range []*store.SharedVolume{lapsed, justEnded} {
		if _, err := env.records.ReserveSharedVolume(context.Background(), row, 100); err != nil {
			t.Fatal(err)
		}
	}
	putOptions := volumestore.PutOptions{MaxBytes: 1 << 20} // 1 MiB
	if _, err := env.local.Put(context.Background(), objectKey("lapsed", volume.KindTar), bytes.NewReader(data), putOptions); err != nil {
		t.Fatal(err)
	}

	env.setClock(start)
	env.service.sweep(context.Background())
	if state := env.stateOf(t, lapsed.ID, lapsed.SHA256); state != store.SharedVolumeFailed {
		t.Fatalf("the lapsed row is %q, want failed by the sweep", state)
	}
	if state := env.stateOf(t, justEnded.ID, justEnded.SHA256); state != store.SharedVolumePublishing {
		t.Fatalf("the row whose lease just ended is %q, want it left to a later sweep", state)
	}
	if !env.hasObject(t, "lapsed", volume.KindTar) {
		t.Fatal("the sweep removed the tar of a row failed just now, before the grace")
	}

	env.setClock(start.Add(env.service.timing.failedGrace))
	env.service.sweep(context.Background())
	if env.hasObject(t, "lapsed", volume.KindTar) {
		t.Fatal("the sweep after the grace left the tar")
	}
	env.setClock(start.Add(2 * env.service.timing.failedGrace))
	env.service.sweep(context.Background())
	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeFailed)); got != 2 {
		t.Errorf("failed rows swept = %v, want 2", got)
	}
}

func TestSweepRemovesADeletedVolumeOnceTheStoreWorks(t *testing.T) {
	env := newTestEnv(t)
	data := validTar(t)
	published, err := env.publish(t, data, "name")
	if err != nil {
		t.Fatal(err)
	}
	env.objects.failDelete = true
	if err := env.service.Delete(context.Background(), digestOf(data), "publisher"); err != nil {
		t.Fatal(err)
	}

	env.service.sweep(context.Background())
	if !env.hasObject(t, published.Volume.ID, volume.KindTar) {
		t.Fatal("a sweep whose deletes failed removed the tar")
	}
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 0 {
		t.Errorf("deleted rows swept = %v after a failed removal, want 0", got)
	}

	env.objects.failDelete = false
	env.service.sweep(context.Background())
	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 1 {
		t.Errorf("deleted rows swept = %v, want 1", got)
	}
}

func TestSweepLeavesLiveVolumesAlone(t *testing.T) {
	env := newTestEnv(t)
	ready, err := env.publish(t, validTar(t), "ready")
	if err != nil {
		t.Fatal(err)
	}
	done := startWaitingPublish(env, makeTar(t, file("in-progress", "x")))
	var publishing *store.SharedVolume
	waitFor(t, func() bool {
		publishing, _ = env.records.GetSharedVolume(context.Background(), digestOf(makeTar(t, file("in-progress", "x"))))
		return publishing != nil
	})

	// Within the publish's lease, and without a grace for failed rows.
	env.service.timing.failedGrace = 0
	env.service.sweep(context.Background())

	if got, err := env.service.Get(context.Background(), ready.Volume.SHA256); err != nil || got.ID != ready.Volume.ID {
		t.Fatalf("Get() of the ready volume = %+v, %v", got, err)
	}
	if !env.hasObject(t, ready.Volume.ID, volume.KindTar) {
		t.Fatal("the sweep removed a ready volume's tar")
	}
	if got, err := env.records.GetSharedVolume(context.Background(), publishing.SHA256); err != nil || got == nil || got.State != store.SharedVolumePublishing {
		t.Fatalf("the publish in progress = %+v, %v; want it publishing", got, err)
	}
	env.shutdown()
	if err := publishError(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
}

func TestSweepRunsUntilItsContextEnds(t *testing.T) {
	env := newTestEnv(t)
	env.service.timing.sweepEvery = 10 * time.Millisecond
	data := validTar(t)
	if _, err := env.publish(t, data, ""); err != nil {
		t.Fatal(err)
	}
	env.objects.failDelete = true
	if err := env.service.Delete(context.Background(), digestOf(data), "admin"); err != nil {
		t.Fatal(err)
	}
	env.objects.failDelete = false

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		env.service.Sweep(ctx)
		close(stopped)
	}()
	waitFor(t, func() bool {
		env.records.mutex.Lock()
		defer env.records.mutex.Unlock()
		return len(env.records.removed) == 1
	})
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Sweep() did not return once its context ended")
	}
	env.assertNothingLeft(t)
}

// deleteTwo publishes two volumes and deletes them, the second a second
// later, leaving both rows deleted with their objects.
func deleteTwo(t *testing.T, env *testEnv) (first, second *store.SharedVolume) {
	t.Helper()
	start := time.Now()
	env.objects.failDelete = true
	var rows []*store.SharedVolume
	for index, data := range [][]byte{validTar(t), makeTar(t, file("second", "x"))} {
		published, err := env.publish(t, data, "")
		if err != nil {
			t.Fatal(err)
		}
		env.setClock(start.Add(time.Duration(index) * time.Second))
		if err := env.service.Delete(context.Background(), published.Volume.SHA256, "publisher"); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, published.Volume)
	}
	env.objects.failDelete = false
	env.setClock(start.Add(2 * time.Second))
	return rows[0], rows[1]
}

func TestSweepGoesPastARowItCannotRemove(t *testing.T) {
	env := newTestEnv(t)
	previous := sweepBatch
	sweepBatch = 1
	t.Cleanup(func() { sweepBatch = previous })
	first, second := deleteTwo(t, env)
	env.objects.failDeleteOf = first.ID

	env.service.sweep(context.Background())

	if state := env.stateOf(t, first.ID, first.SHA256); state != store.SharedVolumeDeleted {
		t.Fatalf("the row whose removal fails is %q, want it deleted still", state)
	}
	if state := env.stateOf(t, second.ID, second.SHA256); state != "" {
		t.Fatalf("the row after it is %q, want it removed", state)
	}
}

func TestSweepStopsWhenItsTimeIsUp(t *testing.T) {
	env := newTestEnv(t)
	deleteTwo(t, env)
	env.objects.deleteHangs = true
	env.service.timing.sweepEvery = 50 * time.Millisecond

	started := time.Now()
	env.service.sweep(context.Background())

	if took := time.Since(started); took >= env.service.timing.cleanup {
		t.Fatalf("the sweep took %s, want it to stop after %s rather than wait out each removal", took, env.service.timing.sweepEvery)
	}
}

// Two instances that sweep one row count it once between them.
func TestSweepCountsOnlyWhatItRemoved(t *testing.T) {
	env := newTestEnv(t)
	deleteTwo(t, env)
	env.records.otherPodRemoves = true

	env.service.sweep(context.Background())

	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 0 {
		t.Errorf("deleted rows swept = %v, want 0: another instance removed them", got)
	}
}
