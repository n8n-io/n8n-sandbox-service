package volumes

import (
	"bytes"
	"context"
	"errors"
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
	if isFailed, err := env.records.FailSharedVolume(context.Background(), "lapsed", start.Unix()); err != nil || !isFailed {
		t.Fatalf("the lapsed row is failed = %v, %v; want true", isFailed, err)
	}
	if got, err := env.records.GetSharedVolume(context.Background(), justEnded.SHA256); err != nil || got == nil || got.State != store.SharedVolumePublishing {
		t.Fatalf("the row whose lease just ended = %+v, %v; want it left to a later sweep", got, err)
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
