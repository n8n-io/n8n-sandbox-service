package volumes

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// stateOf returns the state of the row id, or "" when it is gone. Failed and
// deleted rows are among the purgeable ones; the others are live.
func (env *testEnv) stateOf(t *testing.T, id, sha256 string) store.SharedVolumeState {
	t.Helper()
	rows, err := env.records.ListPurgeableSharedVolumes(context.Background(), 0, store.SharedVolumePosition{}, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row.State
		}
	}
	live, err := env.records.GetLiveSharedVolume(context.Background(), sha256)
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

func TestSweepFailsALapsedPublishAndPurgesItAfterTheGrace(t *testing.T) {
	env := newTestEnv(t)
	// A publish on a pod that died after it stored the tar: its lease ended
	// five minutes ago.
	data := validTar(t)
	lapsed := &store.SharedVolume{ID: "lapsed", SHA256: digestOf(data), Name: "name", PublishedBy: string(RoleVolumePublisher)}
	if _, err := env.records.StartSharedVolumePublish(context.Background(), lapsed, -5*time.Minute, 100); err != nil {
		t.Fatal(err)
	}
	putOptions := volumestore.PutOptions{MaxBytes: 1 << 20} // 1 MiB
	if _, err := env.local.Put(context.Background(), objectKey("lapsed", volume.KindTar), bytes.NewReader(data), putOptions); err != nil {
		t.Fatal(err)
	}

	env.service.sweepOnce(context.Background())
	if state := env.stateOf(t, lapsed.ID, lapsed.SHA256); state != store.SharedVolumeFailed {
		t.Fatalf("the lapsed row is %q, want failed by the sweep", state)
	}
	if !env.hasObject(t, "lapsed", volume.KindTar) {
		t.Fatal("the sweep purged the tar of a row failed just now, before the grace")
	}

	// The grace has passed.
	env.service.timing.failedGrace = 0
	env.service.sweepOnce(context.Background())
	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeFailed)); got != 1 {
		t.Errorf("failed rows swept = %v, want 1", got)
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
	if err := env.service.Delete(context.Background(), digestOf(data), RoleVolumePublisher); err != nil {
		t.Fatal(err)
	}

	env.service.sweepOnce(context.Background())
	if !env.hasObject(t, published.Volume.ID, volume.KindTar) {
		t.Fatal("a sweep whose deletes failed removed the tar")
	}
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 0 {
		t.Errorf("deleted rows swept = %v after a failed purge, want 0", got)
	}
	if got := env.metrics.SweepErrorCount(); got != 1 {
		t.Errorf("sweep errors = %v after a failed purge, want 1", got)
	}

	env.objects.failDelete = false
	env.service.sweepOnce(context.Background())
	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 1 {
		t.Errorf("deleted rows swept = %v, want 1", got)
	}
	if got := env.metrics.SweepErrorCount(); got != 1 {
		t.Errorf("sweep errors = %v after recovery, want 1", got)
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
		publishing, _ = env.records.GetLiveSharedVolume(context.Background(), digestOf(makeTar(t, file("in-progress", "x"))))
		return publishing != nil
	})

	// Within the publish's lease, and without a grace for failed rows.
	env.service.timing.failedGrace = 0
	env.service.sweepOnce(context.Background())

	if got, err := env.service.Get(context.Background(), ready.Volume.SHA256); err != nil || got.ID != ready.Volume.ID {
		t.Fatalf("Get() of the ready volume = %+v, %v", got, err)
	}
	if !env.hasObject(t, ready.Volume.ID, volume.KindTar) {
		t.Fatal("the sweep removed a ready volume's tar")
	}
	if got, err := env.records.GetLiveSharedVolume(context.Background(), publishing.SHA256); err != nil || got == nil || got.State != store.SharedVolumePublishing {
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
	if err := env.service.Delete(context.Background(), digestOf(data), RoleAdmin); err != nil {
		t.Fatal(err)
	}
	env.objects.failDelete = false

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		env.service.RunSweeps(ctx)
		close(stopped)
	}()
	waitFor(t, func() bool {
		env.records.mutex.Lock()
		defer env.records.mutex.Unlock()
		return len(env.records.purged) == 1
	})
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Sweep() did not return once its context ended")
	}
	env.assertNothingLeft(t)
}

// deleteTwoKeepingObjects publishes two volumes and deletes them, leaving both rows deleted
// with their objects. It returns them in the order a sweep lists them.
func deleteTwoKeepingObjects(t *testing.T, env *testEnv) (first, second *store.SharedVolume) {
	t.Helper()
	env.objects.failDelete = true
	for _, data := range [][]byte{validTar(t), makeTar(t, file("second", "x"))} {
		published, err := env.publish(t, data, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := env.service.Delete(context.Background(), published.Volume.SHA256, RoleVolumePublisher); err != nil {
			t.Fatal(err)
		}
	}
	env.objects.failDelete = false
	rows, err := env.records.ListPurgeableSharedVolumes(context.Background(), 0, store.SharedVolumePosition{}, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("purgeable rows = %v, %v; want the two deleted ones", rows, err)
	}
	return rows[0], rows[1]
}

func TestSweepGoesPastARowItCannotPurge(t *testing.T) {
	env := newTestEnv(t)
	previous := sweepBatchRows
	sweepBatchRows = 1
	t.Cleanup(func() { sweepBatchRows = previous })
	first, second := deleteTwoKeepingObjects(t, env)
	env.objects.failDeleteOf = first.ID

	env.service.sweepOnce(context.Background())

	if state := env.stateOf(t, first.ID, first.SHA256); state != store.SharedVolumeDeleted {
		t.Fatalf("the row whose purge fails is %q, want it deleted still", state)
	}
	if state := env.stateOf(t, second.ID, second.SHA256); state != "" {
		t.Fatalf("the row after it is %q, want it purged", state)
	}
}

func TestSweepStopsWhenItsTimeIsUp(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	env := newTestEnv(t)
	deleteTwoKeepingObjects(t, env)
	env.objects.deleteHangs = true
	env.service.timing.sweepEvery = 50 * time.Millisecond

	started := time.Now()
	env.service.sweepOnce(context.Background())

	if took := time.Since(started); took >= env.service.timing.cleanupTimeout {
		t.Fatalf("the sweep took %s, want it to stop after %s rather than wait out each purge", took, env.service.timing.sweepEvery)
	}
	if !strings.Contains(logs.String(), "out of time") {
		t.Fatalf("the sweep logged %q, want a line saying it ran out of time", logs.String())
	}
}

// Two instances that sweep one row count it once between them.
func TestSweepCountsOnlyWhatItPurged(t *testing.T) {
	env := newTestEnv(t)
	deleteTwoKeepingObjects(t, env)
	env.records.otherPodPurges = true

	env.service.sweepOnce(context.Background())

	env.assertNothingLeft(t)
	if got := env.metrics.SweepRemovalCount(string(store.SharedVolumeDeleted)); got != 0 {
		t.Errorf("deleted rows swept = %v, want 0: another instance purged them", got)
	}
}
