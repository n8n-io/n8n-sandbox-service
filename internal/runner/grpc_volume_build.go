package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Results of a BuildVolume call, next to runnerruntime.VolumeBuildReasons: the
// result label of sandbox_volume_builds_total takes only these values.
const (
	volumeBuildSuccess = "success"
	// volumeBuildBusy is a refusal because the runner's one build is running.
	volumeBuildBusy = "busy"
	// volumeBuildRefused is any other refusal before the build: cordoned, not
	// ready, short of disk.
	volumeBuildRefused        = "refused"
	volumeBuildInvalidRequest = "invalid_request"
	volumeBuildCanceled       = "canceled"
)

// volumeImageChunkBytes is far below gRPC's 4 MiB message limit.
const volumeImageChunkBytes = 1 << 20

type volumeBuildStream = grpc.BidiStreamingServer[pb.BuildVolumeRequest, pb.BuildVolumeResponse]

// BuildVolume builds a volume image from the tar the caller streams in and
// streams the image back. Only a runtime that is a VolumeBuilder serves it.
func (s *SandboxControlGRPC) BuildVolume(stream volumeBuildStream) error {
	ctx := stream.Context()
	if err := s.checkAPIKey(ctx); err != nil {
		return err
	}
	builder, ok := s.Runtime.(runnerruntime.VolumeBuilder)
	if !ok {
		return status.Error(codes.Unimplemented, "this runner cannot build volumes")
	}

	now := s.clock()
	tar := &tarStream{stream: stream, now: now}
	err := s.buildVolume(stream, builder, tar)
	result, answer := volumeBuildOutcome(ctx, err)
	s.Rec.ObserveVolumeBuild(result)
	if !tar.firstByte.IsZero() {
		s.Rec.ObserveVolumeBuildDuration(now().Sub(tar.firstByte))
	}
	switch result {
	case volumeBuildSuccess:
	case volumeBuildBusy, volumeBuildRefused:
		slog.InfoContext(ctx, "volume build refused", "result", result, "err", err)
	default:
		slog.WarnContext(ctx, "volume build failed", "result", result, "err", err)
	}
	return answer
}

func (s *SandboxControlGRPC) buildVolume(stream volumeBuildStream, builder runnerruntime.VolumeBuilder, tar *tarStream) error {
	ctx := stream.Context()
	// Refusals come before any tar byte is read, so the API can send the tar
	// to another runner.
	if s.Cordon.Cordoned() {
		return fmt.Errorf("runner is cordoned: %w", runnerruntime.ErrCapacityExhausted)
	}
	if err := s.Runtime.Ready(ctx); err != nil {
		return fmt.Errorf("runner is not ready (%v): %w", err, runnerruntime.ErrCapacityExhausted)
	}

	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "the call ended before its header")
	}
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "the first message must be the header")
	}
	params := volumebuild.Params{
		TarBytes:          header.GetTarBytes(),
		Entries:           header.GetEntries(),
		ContentBytes:      header.GetContentBytes(),
		DecompressedBytes: header.GetDecompressedBytes(),
	}
	if err := params.Validate(); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid header: %v", err)
	}
	tar.want = params.TarBytes

	return builder.BuildVolume(withTrace(ctx), params, tar, func(image io.Reader, size int64) error {
		return sendVolumeImage(stream, image, size)
	})
}

// tarStream reads the tar from the messages after the header. It ends only
// when the caller half-closes after exactly want bytes.
type tarStream struct {
	stream   volumeBuildStream
	now      func() time.Time
	want     int64
	received int64
	chunk    []byte
	// firstByte is when the tar's first byte arrived. Only a build the runtime
	// accepted reads the tar.
	firstByte time.Time
	done      bool
}

func (t *tarStream) Read(buf []byte) (int, error) {
	for len(t.chunk) == 0 {
		if t.done {
			return 0, io.EOF
		}
		msg, err := t.stream.Recv()
		if errors.Is(err, io.EOF) {
			if t.received != t.want {
				return 0, status.Errorf(codes.InvalidArgument, "the tar ended after %d bytes, its header says %d", t.received, t.want)
			}
			t.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		part, ok := msg.GetPart().(*pb.BuildVolumeRequest_TarChunk)
		if !ok {
			return 0, status.Error(codes.InvalidArgument, "only tar chunks may follow the header")
		}
		t.received += int64(len(part.TarChunk))
		if t.received > t.want {
			return 0, status.Errorf(codes.InvalidArgument, "the tar is longer than the %d bytes its header says", t.want)
		}
		if t.firstByte.IsZero() && len(part.TarChunk) > 0 {
			t.firstByte = t.now()
		}
		t.chunk = part.TarChunk
	}
	copied := copy(buf, t.chunk)
	t.chunk = t.chunk[copied:]
	return copied, nil
}

// sendVolumeImage sends the size, the image and its SHA-256, and fails unless
// image holds exactly size bytes.
func sendVolumeImage(stream volumeBuildStream, image io.Reader, size int64) error {
	if err := stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageBytes{ImageBytes: size}}); err != nil {
		return err
	}
	hash := sha256.New()
	var sent int64
	for {
		// A new chunk each time: a sent message must not change.
		chunk := make([]byte, volumeImageChunkBytes)
		read, err := io.ReadFull(image, chunk)
		if read > 0 {
			hash.Write(chunk[:read])
			sent += int64(read)
			if err := stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageChunk{ImageChunk: chunk[:read]}}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read image: %w", err)
		}
	}
	if sent != size {
		return fmt.Errorf("image has %d bytes, want %d", sent, size)
	}
	return stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageSha256{ImageSha256: hex.EncodeToString(hash.Sum(nil))}})
}

// volumeBuildOutcome maps what the build returned to its metric result and
// to the error the caller gets.
func volumeBuildOutcome(ctx context.Context, err error) (result string, answer error) {
	var failed *runnerruntime.VolumeBuildError
	switch {
	case err == nil:
		return volumeBuildSuccess, nil
	case errors.Is(err, runnerruntime.ErrVolumeBuildBusy):
		return volumeBuildBusy, runnerctl.CapacityRefusal(err.Error())
	case errors.Is(err, runnerruntime.ErrCapacityExhausted):
		return volumeBuildRefused, runnerctl.CapacityRefusal(err.Error())
	case errors.As(err, &failed):
		reason, code := failed.Reason, codes.Internal
		switch {
		case !slices.Contains(runnerruntime.VolumeBuildReasons, reason):
			reason = volumebuild.ReasonInternal
		case reason == runnerruntime.VolumeBuildTimeout:
			code = codes.DeadlineExceeded
		case failed.CallerFault():
			code = codes.InvalidArgument
		}
		// Only the reason: the rest may come from the guest.
		return reason, runnerctl.BuildFailure(code, reason, "volume build failed: "+reason)
	case ctx.Err() != nil, errors.Is(err, context.Canceled), status.Code(err) == codes.Canceled:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return volumeBuildCanceled, status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
		}
		return volumeBuildCanceled, status.Error(codes.Canceled, context.Canceled.Error())
	case status.Code(err) == codes.InvalidArgument:
		invalid, _ := status.FromError(err)
		return volumeBuildInvalidRequest, status.Error(codes.InvalidArgument, invalid.Message())
	default:
		// The runner logs err. It names host paths and command output, and
		// the API may pass the message on.
		return volumebuild.ReasonInternal, runnerctl.BuildFailure(codes.Internal, volumebuild.ReasonInternal, "volume build failed: "+volumebuild.ReasonInternal)
	}
}
