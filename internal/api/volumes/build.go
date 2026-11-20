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

// BuildFunc has runner build the image of tar and writes the image to image,
// as runnerctl.BuildVolume does.
type BuildFunc func(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (runnerctl.VolumeImage, error)

// BuildImages makes every later publish build its image with builds before
// the volume is ready: the service's runners attach images. Call it before
// the service takes requests.
func (s *Service) BuildImages(builds Builds) {
	s.builds = &builds
}

var (
	errBuildEnded    = errors.New("the build ended without its image")
	errBuildTimedOut = errors.New("the build call timed out")
)

// storeFailure is a volume store error during a build, which is not the
// runner's fault.
type storeFailure struct{ err error }

func (failure *storeFailure) Error() string { return failure.err.Error() }
func (failure *storeFailure) Unwrap() error { return failure.err }

// checkRunnerBytes refuses a volume that alone takes more on a runner than
// all volumes of a sandbox may, counted as this service's runners store it:
// the image, at the size the builder gives it, when they attach images; the
// tar with its unpacked tree otherwise. row has the tar's size and counts.
func (s *Service) checkRunnerBytes(row *store.SharedVolume) error {
	counts := volume.TarCounts{Entries: row.Entries, ContentBytes: row.ContentBytes, DecompressedBytes: row.DecompressedBytes}
	kind, size := volume.KindTar, row.TarBytes
	if s.builds != nil {
		kind = volume.KindExt4
		size = volumebuild.ImageBytes(volumebuild.Params{
			TarBytes: row.TarBytes, Entries: row.Entries, ContentBytes: row.ContentBytes, DecompressedBytes: row.DecompressedBytes,
		})
		if size > s.limits.maxImageBytes {
			return errImageTooLarge
		}
	}
	if volume.RunnerBytes(kind, size, counts) > s.limits.maxRunnerBytes {
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
		TarBytes: row.TarBytes, Entries: row.Entries, ContentBytes: row.ContentBytes, DecompressedBytes: row.DecompressedBytes,
	}
	giveUp := s.now().Add(s.timing.builderWait)
	wait := s.timing.retryFirst
	cannotBuild := map[string]bool{}
	refusals := 0
	for {
		runners, err := s.builders(cannotBuild)
		if err != nil {
			return volumestore.Object{}, err
		}
		for _, runner := range runners {
			image, err := s.buildOn(ctx, row, runner, header)
			var failedStore *storeFailure
			switch {
			case err == nil:
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildSuccess)
				// Whoever controls the runner chose the image's content.
				slog.InfoContext(ctx, "shared volume image built", "version_id", row.ID, "sha256", row.SHA256,
					"runner_id", runner.ID, "image_bytes", image.Size, "image_sha256", image.SHA256)
				return image, nil
			case ctx.Err() != nil:
				return volumestore.Object{}, ctx.Err()
			case errors.As(err, &failedStore):
				s.metrics.ObserveBuild(metrics.SharedVolumeBuildError)
				return volumestore.Object{}, failedStore.err
			case status.Code(err) == codes.Unimplemented:
				cannotBuild[runner.ID] = true
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
			return volumestore.Object{}, fmt.Errorf("%w: %d refusals, %d runners that can't build", ErrNoCapacity, refusals, len(cannotBuild))
		}
		if err := sleep(ctx, min(wait, remaining)); err != nil {
			return volumestore.Object{}, err
		}
		wait = min(2*wait, s.timing.retryMax)
	}
}

// builders returns the runners a build may go to, the fewest used slots
// first: healthy, not cordoned and heard from within the heartbeat grace. A
// full runner is one: a build takes no slot.
func (s *Service) builders(skip map[string]bool) ([]registry.Runner, error) {
	runners, err := s.builds.Runners.List()
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	now := s.now()
	eligible := runners[:0]
	for _, runner := range runners {
		if runner.Healthy && !runner.Cordoned && runner.ControlGRPCAddr != "" && !skip[runner.ID] &&
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

	sink := &imageSink{put: func(image io.Reader) (volumestore.Object, error) {
		return s.objects.Put(ctx, objectKey(row.ID, volume.KindExt4), image, volumestore.PutOptions{
			MaxBytes: min(s.limits.maxImageBytes, s.limits.maxRunnerBytes),
		})
	}}
	defer sink.abort()
	callCtx, cancel := context.WithTimeout(ctx, s.timing.buildCall)
	built, buildErr := s.builds.Build(callCtx, runner, header, storeReader{reader: tar}, sink)
	if buildErr != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		buildErr = fmt.Errorf("%w after %s: %w", errBuildTimedOut, s.timing.buildCall, buildErr)
	}
	cancel()
	image, err := sink.finish(buildErr)
	switch {
	case err != nil:
		return volumestore.Object{}, err
	case image.Size == 0:
		return volumestore.Object{}, errors.New("the runner sent no image")
	case image.Size != built.Bytes || image.SHA256 != built.SHA256:
		return volumestore.Object{}, fmt.Errorf("the store took %d image bytes with sha256 %s, the runner sent %d with %s", image.Size, image.SHA256, built.Bytes, built.SHA256)
	}
	return image, nil
}

// buildFailure is err, from a call to build on runnerID, as a BuildError
// with a reason from a fixed set.
func buildFailure(runnerID string, err error) *BuildError {
	reason, ok := runnerctl.BuildFailureReason(err)
	switch {
	case ok:
	case errors.Is(err, errBuildTimedOut):
		reason = runnerruntime.VolumeBuildTimeout
	default:
		reason = metrics.SharedVolumeBuildTransport
	}
	return &BuildError{RunnerID: runnerID, Reason: reason, cause: err}
}

// imageSink stores the image a runner sends. Its Put starts at the first
// byte, so a remote store's idle timeout does not run while the runner
// builds, and the image is committed only once finish is told the build
// succeeded.
type imageSink struct {
	put        func(image io.Reader) (volumestore.Object, error)
	pipeWriter *io.PipeWriter
	stored     chan putResult
}

type putResult struct {
	object volumestore.Object
	err    error
}

func (sink *imageSink) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if sink.pipeWriter == nil {
		pipeReader, pipeWriter := io.Pipe()
		sink.pipeWriter = pipeWriter
		sink.stored = make(chan putResult, 1)
		go func() {
			object, err := sink.put(pipeReader)
			// Fails the writes of an image the store stopped reading.
			pipeReader.CloseWithError(err)
			sink.stored <- putResult{object: object, err: err}
		}()
	}
	return sink.pipeWriter.Write(data)
}

// finish commits the image if buildErr is nil, and ends its Put otherwise. A
// Put that failed on its own is a store failure when the build ended for it
// or succeeded, unless the runner sent more than the store was to take.
func (sink *imageSink) finish(buildErr error) (volumestore.Object, error) {
	if sink.pipeWriter == nil {
		return volumestore.Object{}, buildErr
	}
	if buildErr != nil {
		sink.pipeWriter.CloseWithError(errBuildEnded)
	} else {
		_ = sink.pipeWriter.Close()
	}
	result := <-sink.stored
	switch {
	case errors.Is(result.err, volumestore.ErrTooLarge):
		return volumestore.Object{}, fmt.Errorf("the runner sent an image over the cap: %w", result.err)
	case result.err != nil && (buildErr == nil || errors.Is(buildErr, result.err)):
		return volumestore.Object{}, &storeFailure{fmt.Errorf("store the image: %w", result.err)}
	case buildErr != nil:
		return volumestore.Object{}, buildErr
	}
	return result.object, nil
}

// abort ends a Put that finish did not, so its goroutine ends.
func (sink *imageSink) abort() {
	if sink.pipeWriter != nil {
		sink.pipeWriter.CloseWithError(errBuildEnded)
	}
}

// storeReader reads the tar from the store, and marks a failed read as the
// store's rather than the runner's.
type storeReader struct{ reader io.Reader }

func (tar storeReader) Read(buf []byte) (int, error) {
	read, err := tar.reader.Read(buf)
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
