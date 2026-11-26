package volumes

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
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
	env := newTestEnv(t).withBuilds(builds.build,
		builder("full", 4), builder("docker", 0), builder("busy", 1), cordoned, unhealthy, stale, noAddress)

	published, err := env.publish(t, validTar(t), "")
	if err != nil || published.Volume.ImageBytes == 0 {
		t.Fatalf("Publish() = %+v, %v; want an image", published, err)
	}
	if calls := builds.callsMade(); !slices.Equal(calls, []string{"docker", "busy", "full"}) {
		t.Fatalf("runners called = %v; want the ones that may build, fewest used slots first", calls)
	}
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildRefused); got != 1 {
		t.Errorf("refused builds = %v, want 1", got)
	}
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildUnsupported); got != 1 {
		t.Errorf("runners that can't build = %v, want 1", got)
	}
}

func TestBuildWaitsForARunnerThenGivesUp(t *testing.T) {
	builds := newFakeBuilds(testImage())
	builds.refusalsByRunnerID["a"] = runnerctl.CapacityRefusal("busy")
	env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))

	if _, err := env.publish(t, validTar(t), "name"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Publish() = %v, want ErrNoCapacity", err)
	}
	if calls := builds.callsMade(); len(calls) < 2 {
		t.Errorf("runners called = %v; want retries after a wait", calls)
	}
	env.assertNothingLeft(t)
	if got := env.metrics.PublishCount(metrics.SharedVolumePublishNoCapacity); got != 1 {
		t.Errorf("no_capacity publishes = %v, want 1", got)
	}

	none := newTestEnv(t).withBuilds(builds.build)
	if _, err := none.publish(t, validTar(t), ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Publish() without runners = %v, want ErrNoCapacity", err)
	}
	none.assertNothingLeft(t)
}

func TestAFailedBuildFailsThePublish(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		configure   func(*fakeBuilds)
		reason      string
		callerFault bool
	}{
		{"a tar the builder refuses", func(builds *fakeBuilds) {
			builds.fail = runnerctl.BuildFailure(codes.InvalidArgument, "tar", "failed: tar")
		}, "tar", true},
		{"fsck", func(builds *fakeBuilds) { builds.fail = runnerctl.BuildFailure(codes.Internal, "fsck", "failed: fsck") }, "fsck", false},
		{"a reason no runner has", func(builds *fakeBuilds) {
			builds.fail = runnerctl.BuildFailure(codes.Internal, `x",result="success`, "failed")
		}, "internal", false},
		{"a broken call", func(builds *fakeBuilds) { builds.fail = status.Error(codes.Unavailable, "connection reset") }, "transport", false},
		{"another digest", func(builds *fakeBuilds) { builds.claim = digestOf([]byte("another image")) }, "transport", false},
		{"no image", func(builds *fakeBuilds) { builds.image = nil }, "transport", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			builds := newFakeBuilds(testImage())
			testCase.configure(builds)
			env := newTestEnv(t).withBuilds(builds.build, builder("a", 0), builder("b", 1))

			_, err := env.publish(t, validTar(t), "")

			var failure *ImageBuildError
			if !errors.As(err, &failure) || failure.Reason != testCase.reason || failure.CallerFault() != testCase.callerFault || failure.RunnerID != "a" {
				t.Fatalf("Publish() = %v; want a build error on runner a with reason %s, caller fault %v", err, testCase.reason, testCase.callerFault)
			}
			if calls := builds.callsMade(); !slices.Equal(calls, []string{"a"}) {
				t.Fatalf("runners called = %v; a failed build is not tried again", calls)
			}
			env.assertNothingLeft(t)
			if got := env.metrics.BuildCount(testCase.reason); got != 1 {
				t.Errorf("builds with result %s = %v, want 1", testCase.reason, got)
			}
			if got := env.metrics.PublishCount(metrics.SharedVolumePublishBuildFailed); got != 1 {
				t.Errorf("build_failed publishes = %v, want 1", got)
			}
		})
	}
}

func TestAStoreFailureDuringTheBuild(t *testing.T) {
	builds := newFakeBuilds(testImage())
	env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
	env.objects.failImagePut = true

	_, err := env.publish(t, validTar(t), "")

	if err == nil || errors.Is(err, ErrBuildFailed) {
		t.Fatalf("Publish() = %v, want a store error, not a failed build", err)
	}
	env.assertNothingLeft(t)
	if got := env.metrics.BuildCount(metrics.SharedVolumeBuildError); got != 1 {
		t.Errorf("builds with result error = %v, want 1", got)
	}
	if got := env.metrics.PublishCount(metrics.SharedVolumePublishError); got != 1 {
		t.Errorf("error publishes = %v, want 1", got)
	}
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
			t.Fatalf("Publish() = %v, want ErrTooLarge", err)
		}
		env.assertNothingLeft(t)

		env.service.limits.maxRunnerBytes = need
		if _, err := env.publish(t, data, "name"); err != nil {
			t.Fatalf("Publish() at the cap = %v", err)
		}
	})

	t.Run("an image over the image cap", func(t *testing.T) {
		builds := newFakeBuilds(testImage())
		env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
		env.service.limits.maxImageBytes = imageBytesOf(data, counts) - 1
		if _, err := env.publish(t, data, ""); !errors.Is(err, ErrTooLarge) || len(builds.callsMade()) != 0 {
			t.Fatalf("Publish() = %v, calls = %v; want ErrTooLarge without calling a runner", err, builds.callsMade())
		}
		env.assertNothingLeft(t)
	})

	t.Run("an image over what a sandbox may take", func(t *testing.T) {
		builds := newFakeBuilds(testImage())
		env := newTestEnv(t).withBuilds(builds.build, builder("a", 0))
		env.service.limits.maxRunnerBytes = imageBytesOf(data, counts) - 1
		if _, err := env.publish(t, data, ""); !errors.Is(err, ErrTooLarge) || len(builds.callsMade()) != 0 {
			t.Fatalf("Publish() = %v, calls = %v; want ErrTooLarge without calling a runner", err, builds.callsMade())
		}
		env.assertNothingLeft(t)
	})
}
