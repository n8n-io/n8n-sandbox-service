package volumes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

func testImage() []byte {
	return bytes.Repeat([]byte("ext4 image "), 30000)
}

func countsOf(t *testing.T, data []byte) volume.TarCounts {
	t.Helper()
	stats, err := safetar.Validate(bytes.NewReader(data), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return volume.TarCounts{Entries: int64(stats.Entries), ContentBytes: stats.Bytes, DecompressedBytes: stats.DecompressedBytes}
}

func imageBytesOf(data []byte, counts volume.TarCounts) int64 {
	return volumebuild.ImageBytes(volumebuild.Params{
		TarBytes: int64(len(data)), Entries: counts.Entries, ContentBytes: counts.ContentBytes, DecompressedBytes: counts.DecompressedBytes,
	})
}

func TestBuildStoresTheImageUnderTheDigestTheAPIComputed(t *testing.T) {
	builds := newFakeBuilds(testImage())
	env := newTestEnv(t).withBuilds(builds.build, builder("runner-a", 0))
	data := gzipped(t, validTar(t))

	published, err := env.publish(t, data, "with-image")
	if err != nil || !published.Created {
		t.Fatalf("Publish() = %+v, %v", published, err)
	}
	row := published.Volume
	image := testImage()
	if row.ImageBytes != int64(len(image)) || row.ImageSHA256 != digestOf(image) {
		t.Fatalf("row image = %d bytes, %s; want %d, %s", row.ImageBytes, row.ImageSHA256, len(image), digestOf(image))
	}
	stored, err := env.local.Stat(context.Background(), objectKey(row.ID, volume.KindExt4))
	if err != nil || stored.Size != row.ImageBytes || stored.SHA256 != row.ImageSHA256 {
		t.Fatalf("stored image = %+v, %v; want the row's", stored, err)
	}
	if _, err := env.local.Stat(context.Background(), objectKey(row.ID, volume.KindTar)); err != nil {
		t.Fatalf("the tar = %v; want it kept next to the image", err)
	}

	// The runner got the stored tar, with its length and counts.
	if len(builds.tars) != 1 || !bytes.Equal(builds.tars[0], data) {
		t.Fatal("the runner did not get the tar as published")
	}
	header := builds.headers[0]
	counts := countsOf(t, data)
	if header.GetTarBytes() != int64(len(data)) || header.GetEntries() != counts.Entries ||
		header.GetContentBytes() != counts.ContentBytes || header.GetDecompressedBytes() != counts.DecompressedBytes {
		t.Fatalf("header = %+v, want %d tar bytes and counts %+v", header, len(data), counts)
	}
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildSuccess); got != 1 {
		t.Errorf("successful builds = %v, want 1", got)
	}

	if err := env.service.Delete(context.Background(), digestOf(data), RoleVolumePublisher); err != nil {
		t.Fatal(err)
	}
	env.assertNothingLeft(t)
}

func TestBuildGoesToTheLeastUsedRunnerThatCanTakeIt(t *testing.T) {
	builds := newFakeBuilds(testImage())
	builds.refusalsByRunnerID["docker"] = status.Error(codes.Unimplemented, "this runner cannot build volumes")
	builds.refusalsByRunnerID["busy"] = runnerctl.CapacityRefusal("a volume build is already running")
	cordoned, unhealthy, stale, noAddress := builder("cordoned", 0), builder("unhealthy", 0), builder("stale", 0), builder("no-address", 0)
	cordoned.Cordoned, unhealthy.Healthy, stale.LastSeen, noAddress.ControlGRPCAddr = true, false, time.Now().Add(-time.Hour), ""
	full := builder("full", 4)
	env := newTestEnv(t).withBuilds(builds.build,
		full, builder("docker", 0), builder("busy", 1), cordoned, unhealthy, stale, noAddress)

	published, err := env.publish(t, validTar(t), "")
	if err != nil || published.Volume.ImageBytes == 0 {
		t.Fatalf("Publish() = %+v, %v; want an image", published, err)
	}
	if calls := builds.callsMade(); !slices.Equal(calls, []string{"docker", "busy", "full"}) {
		t.Fatalf("runners called = %v; want the ones that may build, fewest used slots first", calls)
	}
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildRefused); got != 1 {
		t.Errorf("refused builds = %v, want 1: a runner that can't build is no attempt", got)
	}
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildUnsupported); got != 1 {
		t.Errorf("runners that can't build = %v, want 1", got)
	}
}

func TestBuildWaitsForARunnerThenGivesUp(t *testing.T) {
	builds := newFakeBuilds(testImage())
	builds.refusalsByRunnerID["a"] = runnerctl.CapacityRefusal("busy")
	builds.refusalsByRunnerID["b"] = runnerctl.CapacityRefusal("cordoned")
	env := newTestEnv(t).withBuilds(builds.build, builder("a", 0), builder("b", 0))
	started := time.Now()

	_, err := env.publish(t, validTar(t), "name")

	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Publish() = %v, want ErrNoCapacity", err)
	}
	if waited := time.Since(started); waited < env.service.timing.builderWait {
		t.Errorf("gave up after %s, want at least %s", waited, env.service.timing.builderWait)
	}
	if calls := builds.callsMade(); len(calls) < 4 {
		t.Errorf("runners called = %v; want them tried again after a wait", calls)
	}
	env.assertNothingLeft(t)
	if got := env.metrics.PublishCount(metrics.SharedVolumePublishNoCapacity); got != 1 {
		t.Errorf("no_capacity publishes = %v, want 1", got)
	}

	// Without any runner it ends the same way.
	none := newTestEnv(t).withBuilds(builds.build)
	if _, err := none.publish(t, validTar(t), ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Publish() without runners = %v, want ErrNoCapacity", err)
	}
	none.assertNothingLeft(t)
}

func TestAFailedBuildFailsThePublish(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		reason      string
		callerFault bool
	}{
		{"a tar the builder refuses", runnerctl.BuildFailure(codes.InvalidArgument, "tar", "volume build failed: tar"), "tar", true},
		{"a tar over its counts", runnerctl.BuildFailure(codes.InvalidArgument, "too_large", "volume build failed: too_large"), "too_large", true},
		{"an image out of space", runnerctl.BuildFailure(codes.InvalidArgument, "no_space", "volume build failed: no_space"), "no_space", true},
		{"fsck", runnerctl.BuildFailure(codes.Internal, "fsck", "volume build failed: fsck"), "fsck", false},
		{"the runner's time limit", runnerctl.BuildFailure(codes.DeadlineExceeded, "timeout", "volume build failed: timeout"), "timeout", false},
		{"a reason no runner has", runnerctl.BuildFailure(codes.Internal, `x",result="success`, "failed"), "internal", false},
		{"a broken call", status.Error(codes.Unavailable, "connection reset"), "transport", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builds := newFakeBuilds(testImage())
			builds.fail = tc.err
			env := newTestEnv(t).withBuilds(builds.build, builder("a", 0), builder("b", 1))

			_, err := env.publish(t, validTar(t), "")

			var failure *BuildError
			if !errors.As(err, &failure) || failure.Reason != tc.reason || failure.CallerFault() != tc.callerFault || failure.RunnerID != "a" {
				t.Fatalf("Publish() = %v; want a build error on runner a with reason %s, caller fault %v", err, tc.reason, tc.callerFault)
			}
			if calls := builds.callsMade(); !slices.Equal(calls, []string{"a"}) {
				t.Fatalf("runners called = %v; a failed build is not tried again", calls)
			}
			env.assertNothingLeft(t)
			if got := env.metrics.BuildCount(tc.reason); got != 1 {
				t.Errorf("builds with result %s = %v, want 1", tc.reason, got)
			}
			if got := env.metrics.PublishCount(metrics.SharedVolumePublishBuildFailed); got != 1 {
				t.Errorf("build_failed publishes = %v, want 1", got)
			}
		})
	}
}

func TestBuildTimesOut(t *testing.T) {
	env := newTestEnv(t)
	env.service.timing.buildCall = 50 * time.Millisecond
	env.withBuilds(func(ctx context.Context, _ registry.Runner, _ *pb.BuildVolumeHeader, _ io.Reader, _ io.Writer) (runnerctl.VolumeImage, error) {
		<-ctx.Done()
		return runnerctl.VolumeImage{}, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
	}, builder("a", 0))

	_, err := env.publish(t, validTar(t), "")

	var failure *BuildError
	if !errors.As(err, &failure) || failure.Reason != "timeout" || failure.CallerFault() {
		t.Fatalf("Publish() = %v, want a build error with reason timeout", err)
	}
	env.assertNothingLeft(t)
}

func TestBuildWithAnImageOtherThanTheRunnerSays(t *testing.T) {
	for name, configure := range map[string]func(*fakeBuilds){
		"another digest": func(builds *fakeBuilds) { builds.claim = digestOf([]byte("another image")) },
		"no image":       func(builds *fakeBuilds) { builds.image = nil },
	} {
		t.Run(name, func(t *testing.T) {
			builds := newFakeBuilds(testImage())
			configure(builds)
			env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))

			_, err := env.publish(t, validTar(t), "")

			var failure *BuildError
			if !errors.As(err, &failure) || failure.Reason != "transport" {
				t.Fatalf("Publish() = %v, want a build error with reason transport", err)
			}
			env.assertNothingLeft(t)
		})
	}
}

func TestImagePutStartsAtTheFirstImageByte(t *testing.T) {
	builds := newFakeBuilds(testImage())
	env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
	puts := -1
	builds.beforeImage = func() { puts = env.objects.putsOf(volume.KindExt4) }

	if _, err := env.publish(t, validTar(t), ""); err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Fatalf("%d image Puts had started before the image's first byte, want none", puts)
	}
	if got := env.objects.putsOf(volume.KindExt4); got != 1 {
		t.Fatalf("%d image Puts, want 1", got)
	}
}

func TestAStoreFailureDuringTheBuild(t *testing.T) {
	for name, configure := range map[string]func(*failingObjects){
		"storing the image": func(objects *failingObjects) { objects.failImagePut = true },
		"reading the tar":   func(objects *failingObjects) { objects.failTarRead = true },
	} {
		t.Run(name, func(t *testing.T) {
			builds := newFakeBuilds(testImage())
			env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
			configure(env.objects)

			_, err := env.publish(t, validTar(t), "")

			if err == nil || errors.Is(err, ErrBuildFailed) {
				t.Fatalf("Publish() = %v, want a store error, not a failed build", err)
			}
			env.objects.failTarRead = false
			env.assertNothingLeft(t)
			if got := env.metrics.BuildCount(metrics.SharedVolumeBuildError); got != 1 {
				t.Errorf("builds with result error = %v, want 1", got)
			}
			if got := env.metrics.PublishCount(metrics.SharedVolumePublishError); got != 1 {
				t.Errorf("error publishes = %v, want 1", got)
			}
		})
	}
}

// A runner that fails on its own while the store waits for the rest of the
// image is the runner's failure, whatever the store's Put then says.
func TestARunnerFailureWhileTheImageIsStored(t *testing.T) {
	sink := &imageSink{put: func(image io.Reader) (volumestore.Object, error) {
		_, err := io.Copy(io.Discard, image)
		return volumestore.Object{}, fmt.Errorf("stalled: %w", err)
	}}
	if _, err := sink.Write([]byte("part of an image")); err != nil {
		t.Fatal(err)
	}
	runnerFailed := status.Error(codes.DeadlineExceeded, "deadline exceeded")
	_, err := sink.finishPut(runnerFailed)
	var failedStore *storeFailure
	if !errors.Is(err, runnerFailed) || errors.As(err, &failedStore) {
		t.Fatalf("finish() = %v, want the runner's error", err)
	}
}

// The build goes on without the client: it has sent the whole tar.
func TestAPublisherThatGoesAwayDuringTheBuild(t *testing.T) {
	builds := newFakeBuilds(testImage())
	env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
	data := validTar(t)
	requestCtx, cancel := context.WithCancel(context.Background())
	builds.beforeImage = cancel

	published, err := env.service.Publish(requestCtx, PublishRequest{
		SHA256: digestOf(data), SizeBytes: int64(len(data)), Body: bytes.NewReader(data), Role: RoleVolumePublisher,
	})

	if err != nil || !published.Created || published.Volume.ImageBytes == 0 {
		t.Fatalf("Publish() = %+v, %v; want the volume ready with its image", published, err)
	}
	if _, err := env.service.Get(context.Background(), digestOf(data)); err != nil {
		t.Fatalf("Get() = %v", err)
	}
}

func TestShutdownInterruptsABuild(t *testing.T) {
	builds := newFakeBuilds(testImage())
	env := newTestEnv(t)
	builds.beforeImage = env.shutdown
	env.withBuilds(builds.build, builder("a", 0))

	_, err := env.publish(t, validTar(t), "")

	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Publish() = %v, want ErrInterrupted", err)
	}
	env.assertNothingLeft(t)
}

// A service counts a volume as its runners store it: a tar with its unpacked
// tree, or the image alone.
func TestPublishRefusesAVolumeNoSandboxCouldTake(t *testing.T) {
	data := validTar(t)
	counts := countsOf(t, data)

	t.Run("a tar and its tree", func(t *testing.T) {
		need := volume.RunnerBytes(volume.KindTar, int64(len(data)), counts)
		env := newTestEnv(t)
		env.service.limits.maxRunnerBytes = need - 1
		if _, err := env.publish(t, data, "name"); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Publish() of %d bytes on a runner over a cap of %d = %v, want ErrTooLarge", need, need-1, err)
		}
		env.assertNothingLeft(t)
		if got := env.metrics.PublishCount(metrics.SharedVolumePublishTooLarge); got != 1 {
			t.Errorf("too_large publishes = %v, want 1", got)
		}

		env.service.limits.maxRunnerBytes = need
		if _, err := env.publish(t, data, "name"); err != nil {
			t.Fatalf("Publish() at the cap = %v", err)
		}
	})

	t.Run("an image over the image cap", func(t *testing.T) {
		builds := newFakeBuilds(testImage())
		env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
		env.service.limits.maxImageBytes = imageBytesOf(data, counts) - 1
		if _, err := env.publish(t, data, ""); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Publish() = %v, want ErrTooLarge", err)
		}
		if calls := builds.callsMade(); len(calls) != 0 {
			t.Fatalf("runners called = %v, want none", calls)
		}
		env.assertNothingLeft(t)

		env.service.limits.maxImageBytes = imageBytesOf(data, counts)
		if _, err := env.publish(t, data, ""); err != nil {
			t.Fatalf("Publish() at the image cap = %v", err)
		}
	})

	t.Run("an image over what a sandbox may take", func(t *testing.T) {
		builds := newFakeBuilds(testImage())
		env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
		env.service.limits.maxRunnerBytes = imageBytesOf(data, counts) - 1
		if _, err := env.publish(t, data, ""); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Publish() = %v, want ErrTooLarge", err)
		}
		if calls := builds.callsMade(); len(calls) != 0 {
			t.Fatalf("runners called = %v, want none", calls)
		}
		env.assertNothingLeft(t)
	})
}

// A gzip tar of little content that unpacks to a lot of padding takes more as
// a tree than as an image: only the kind the runners attach counts.
func TestOnlyTheKindTheRunnersAttachCounts(t *testing.T) {
	// 10 MiB of gzip with 1 MiB of content, which unpacks to 8 GiB.
	row := &store.SharedVolume{TarBytes: 10 << 20, TarEntries: 10, TarContentBytes: 1 << 20, TarDecompressedBytes: 8 << 30} // 10 MiB tar, 1 MiB content, 8 GiB decompressed
	counts := volume.TarCounts{Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes}
	tarNeed := volume.RunnerBytes(volume.KindTar, row.TarBytes, counts)
	imageNeed := volumebuild.ImageBytes(volumebuild.Params{TarBytes: row.TarBytes, Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes})
	if imageNeed >= tarNeed {
		t.Fatalf("the image takes %d bytes, the tar %d; the test needs a smaller image", imageNeed, tarNeed)
	}

	env := newTestEnv(t)
	env.service.limits.maxRunnerBytes = imageNeed
	if err := env.service.checkRunnerBytes(row); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("checkRunnerBytes() for tar runners = %v, want ErrTooLarge", err)
	}
	env.withBuilds(newFakeBuilds(nil).build)
	if err := env.service.checkRunnerBytes(row); err != nil {
		t.Fatalf("checkRunnerBytes() for image runners = %v, want nil: they never get the tar", err)
	}
}
