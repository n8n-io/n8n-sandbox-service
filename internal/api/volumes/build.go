// This file has runners build the ext4 image of each shared volume.

package volumes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// Builds has runners build the ext4 image of each shared volume as it is
// published.
type Builds struct {
	// Runners lists the runners a build may go to.
	Runners Runners
	// Build calls one runner.
	Build BuildFunc
	// HeartbeatGrace is how long after its last heartbeat a runner still gets
	// builds.
	HeartbeatGrace time.Duration
}

// Runners lists the registered runners; a registry.RunnerRegistry is one.
type Runners interface {
	List() ([]registry.Runner, error)
}

// BuildFunc has runner build the image of tar and write it to output,
// as runnerctl.BuildVolume does.
type BuildFunc func(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, output io.Writer) (runnerctl.VolumeImage, error)

// EnableImageBuilds makes each later publish have a runner build the image of
// the volume before the volume is ready. Call it before the service takes
// requests.
func (s *Service) EnableImageBuilds(builds Builds) {
	s.builds = &builds
}

var errBuildTimedOut = errors.New("the build call timed out")

// storeFailure is a volume store error during a build, which is not the
// runner's fault.
type storeFailure struct{ err error }

func (failure *storeFailure) Error() string { return failure.err.Error() }
func (failure *storeFailure) Unwrap() error { return failure.err }

// checkRunnerBytes refuses a volume that alone needs more room on a runner
// than all volumes of one sandbox may take. It counts the volume as the
// runners keep it: the estimated image when they attach images, else the tar
// and its unpacked tree. row must hold the tar's size and counts.
func (s *Service) checkRunnerBytes(row *store.SharedVolume) error {
	counts := volume.TarCounts{Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes}
	kind, objectBytes := volume.KindTar, row.TarBytes
	if s.builds != nil {
		kind = volume.KindExt4
		objectBytes = volumebuild.ImageBytes(volumebuild.Params{
			TarBytes: row.TarBytes, Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes,
		})
		if objectBytes > s.limits.maxImageBytes {
			return errImageTooLarge
		}
	}
	if volume.RunnerBytes(kind, objectBytes, counts) > s.limits.maxRunnerBytes {
		return errTooLargeToRun
	}
	return nil
}

// build has a runner build the image of row's stored tar, stores the image
// and returns it as the store counted and hashed it. A runner that refuses
// for capacity passes the build on to the next; once all have refused, they
// are tried again after a wait, until timing.builderWait has passed. A
// runner that can't build is skipped. A build that fails is not tried again.
func (s *Service) build(ctx context.Context, row *store.SharedVolume) (volumestore.Object, error) {
	header := &pb.BuildVolumeHeader{
		TarBytes: row.TarBytes, Entries: row.TarEntries, ContentBytes: row.TarContentBytes, DecompressedBytes: row.TarDecompressedBytes,
	}
	giveUp := s.now().Add(s.timing.builderWait)
	wait := s.timing.retryFirst
	cannotBuildRunnerIDs := map[string]struct{}{}
	refusals := 0
	for {
		runners, err := s.listBuildRunners(cannotBuildRunnerIDs)
		if err != nil {
			return volumestore.Object{}, err
		}
		for _, runner := range runners {
			image, err := s.buildOn(ctx, row, runner, header)
			var failedStore *storeFailure
			switch {
			case err == nil:
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildSuccess)
				// Log the runner, because only the runner decides what the image
				// contains.
				slog.InfoContext(ctx, "shared volume image built", "version_id", row.ID, "sha256", row.SHA256,
					"runner_id", runner.ID, "image_bytes", image.Size, "image_sha256", image.SHA256)
				return image, nil
			case ctx.Err() != nil:
				return volumestore.Object{}, ctx.Err()
			case errors.As(err, &failedStore):
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildError)
				return volumestore.Object{}, failedStore.err
			case status.Code(err) == codes.Unimplemented:
				cannotBuildRunnerIDs[runner.ID] = struct{}{}
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildUnsupported)
				slog.WarnContext(ctx, "shared volume build: runner cannot build", "version_id", row.ID, "runner_id", runner.ID)
			case runnerctl.IsCapacityRefusal(err):
				refusals++
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildRefused)
				slog.InfoContext(ctx, "shared volume build: runner refused",
					"version_id", row.ID, "sha256", row.SHA256, "runner_id", runner.ID, "refusals", refusals)
			default:
				failure := buildFailure(runner.ID, err)
				s.metrics.ObserveBuild(failure.Reason)
				return volumestore.Object{}, failure
			}
		}
		remaining := giveUp.Sub(s.now())
		if remaining <= 0 {
			return volumestore.Object{}, fmt.Errorf("%w: %d refusals, %d runners that can't build", ErrNoCapacity, refusals, len(cannotBuildRunnerIDs))
		}
		if err := sleep(ctx, min(wait, remaining)); err != nil {
			return volumestore.Object{}, err
		}
		wait = min(2*wait, s.timing.retryMax)
	}
}

// listBuildRunners returns the runners that may take a build, the fewest used
// slots first. Such a runner is healthy, not cordoned, not in skip, has a
// control address and sent a heartbeat within the grace. A full runner may
// also take a build, because a build uses no sandbox slot.
func (s *Service) listBuildRunners(skip map[string]struct{}) ([]registry.Runner, error) {
	runners, err := s.builds.Runners.List()
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	now := s.now()
	eligible := runners[:0]
	for _, runner := range runners {
		_, skipped := skip[runner.ID]
		if runner.Healthy && !runner.Cordoned && runner.ControlGRPCAddr != "" && !skipped &&
			now.Sub(runner.LastSeen) <= s.builds.HeartbeatGrace {
			eligible = append(eligible, runner)
		}
	}
	slices.SortStableFunc(eligible, func(first, second registry.Runner) int {
		return cmp.Compare(first.CapacityUsed, second.CapacityUsed)
	})
	return eligible, nil
}

// buildOn has runner build the image of row's tar, read from the store, and
// stores the image as it arrives. It fails with a *storeFailure for the
// store, and with the call's error otherwise.
func (s *Service) buildOn(ctx context.Context, row *store.SharedVolume, runner registry.Runner, header *pb.BuildVolumeHeader) (volumestore.Object, error) {
	tar, _, err := s.objects.Open(ctx, objectKey(row.ID, volume.KindTar))
	if err != nil {
		return volumestore.Object{}, &storeFailure{fmt.Errorf("open the tar: %w", err)}
	}
	defer tar.Close()

	sink := newImageSink(func(image io.Reader) (volumestore.Object, error) {
		return s.objects.Put(ctx, objectKey(row.ID, volume.KindExt4), image, volumestore.PutOptions{
			MaxBytes: min(s.limits.maxImageBytes, s.limits.maxRunnerBytes),
		})
	})
	defer sink.abortPut()
	callCtx, cancel := context.WithTimeout(ctx, s.timing.buildCall)
	reportedImage, buildError := s.builds.Build(callCtx, runner, header, storeReader{reader: tar}, sink)
	if buildError != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		buildError = fmt.Errorf("%w after %s: %w", errBuildTimedOut, s.timing.buildCall, buildError)
	}
	cancel()
	storedImage, err := sink.finishPut(buildError)
	switch {
	case err != nil:
		return volumestore.Object{}, err
	case storedImage.Size == 0:
		return volumestore.Object{}, errors.New("the runner sent no image")
	case storedImage.Size != reportedImage.Bytes || storedImage.SHA256 != reportedImage.SHA256:
		return volumestore.Object{}, fmt.Errorf("the store took %d image bytes with sha256 %s, the runner sent %d with %s", storedImage.Size, storedImage.SHA256, reportedImage.Bytes, reportedImage.SHA256)
	}
	return storedImage, nil
}

// buildFailure is err, from a call to build on runnerID, as an ImageBuildError
// with a reason from a fixed set.
func buildFailure(runnerID string, err error) *ImageBuildError {
	reason, ok := runnerctl.BuildFailureReason(err)
	switch {
	case ok:
	case errors.Is(err, errBuildTimedOut):
		reason = runnerruntime.VolumeBuildTimeout
	default:
		reason = metrics.SharedVolumeBuildTransport
	}
	return &ImageBuildError{RunnerID: runnerID, Reason: reason, cause: err}
}

// storeReader reads the tar from the store, and marks a failed read as the
// store's rather than the runner's.
type storeReader struct{ reader io.Reader }

func (tar storeReader) Read(buffer []byte) (int, error) {
	read, err := tar.reader.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return read, &storeFailure{fmt.Errorf("read the tar: %w", err)}
	}
	return read, err
}

// sleep waits for wait, or until ctx ends.
func sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
