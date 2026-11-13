package runnerctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// A runner marks a volume build that ran and failed with this ErrorInfo, and
// puts its reason into the metadata. The code alone does not tell a bad tar
// from a call that failed on the way.
const (
	buildFailureReason      = "VOLUME_BUILD_FAILED"
	buildFailureMetadataKey = "reason"
)

// BuildFailure is what a runner returns for a volume build that ran and
// failed. reason is one of the runner's fixed reasons, such as "tar".
func BuildFailure(code codes.Code, reason, msg string) error {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{
		Domain:   refusalDomain,
		Reason:   buildFailureReason,
		Metadata: map[string]string{buildFailureMetadataKey: reason},
	})
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

// BuildFailureReason returns the runner's reason if err is a BuildFailure. A
// reason that is not one of runnerruntime.VolumeBuildReasons comes back as
// "internal": callers use it as a metric label.
func BuildFailureReason(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return "", false
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == refusalDomain && info.GetReason() == buildFailureReason {
			reason, ok := info.GetMetadata()[buildFailureMetadataKey]
			if !ok || reason == "" {
				return "", false
			}
			if !slices.Contains(runnerruntime.VolumeBuildReasons, reason) {
				reason = volumebuild.ReasonInternal
			}
			return reason, true
		}
	}
	return "", false
}

// VolumeImage describes an image BuildVolume received.
type VolumeImage struct {
	Bytes  int64
	SHA256 string
}

// volumeChunkBytes is far below gRPC's 4 MiB message limit.
const volumeChunkBytes = 1 << 20

// BuildVolume calls SandboxControl.BuildVolume and closes the connection. It
// sends the header and the tar, and writes the image to image as it arrives.
// It fails unless the image has the size and SHA-256 the runner announced;
// image may then hold part of it. It has stopped reading tar when it returns,
// so the caller can rewind tar for another runner: it waits for a read of tar
// that is under way, whatever ctx says.
//
// A runner without capacity for the build answers with a CapacityRefusal, a
// build that failed with a BuildFailure.
func BuildVolume(ctx context.Context, target, apiKey string, tlsCfg *TLS, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (VolumeImage, error) {
	opts, err := dialOpts(target, tlsCfg)
	if err != nil {
		return VolumeImage{}, err
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return VolumeImage{}, fmt.Errorf("runnerctl dial: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(withCallMetadata(ctx, apiKey))
	defer cancel()
	stream, err := pb.NewSandboxControlClient(conn).BuildVolume(ctx)
	if err != nil {
		return VolumeImage{}, err
	}

	sent := make(chan error, 1)
	go func() {
		err := sendVolumeTar(stream, header, tar)
		if err != nil {
			cancel()
		}
		sent <- err
	}()
	result, err := receiveVolumeImage(stream, image)
	if err != nil {
		// Ends a send that waits for a runner that has already answered.
		cancel()
	}
	if tarErr := <-sent; tarErr != nil {
		// Reading the tar failed here, which is what cancelled the call.
		return VolumeImage{}, tarErr
	}
	if err != nil {
		return VolumeImage{}, err
	}
	return result, nil
}

// sendVolumeTar returns an error only for a tar it could not read. A send that
// fails means the runner ended the call, and its answer says why.
func sendVolumeTar(stream grpc.BidiStreamingClient[pb.BuildVolumeRequest, pb.BuildVolumeResponse], header *pb.BuildVolumeHeader, tar io.Reader) error {
	if err := stream.Send(&pb.BuildVolumeRequest{Part: &pb.BuildVolumeRequest_Header{Header: header}}); err != nil {
		return nil
	}
	chunk := make([]byte, volumeChunkBytes)
	for {
		read, err := io.ReadFull(tar, chunk)
		if read > 0 {
			if sendErr := stream.Send(&pb.BuildVolumeRequest{Part: &pb.BuildVolumeRequest_TarChunk{TarChunk: chunk[:read]}}); sendErr != nil {
				return nil
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			_ = stream.CloseSend()
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
	}
}

func receiveVolumeImage(stream grpc.BidiStreamingClient[pb.BuildVolumeRequest, pb.BuildVolumeResponse], image io.Writer) (VolumeImage, error) {
	first, err := stream.Recv()
	if err != nil {
		return VolumeImage{}, err
	}
	size, ok := first.GetPart().(*pb.BuildVolumeResponse_ImageBytes)
	if !ok || size.ImageBytes < 0 {
		return VolumeImage{}, errors.New("runner did not start its answer with the image size")
	}

	hash := sha256.New()
	var received int64
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return VolumeImage{}, errors.New("runner ended its answer without the image digest")
		}
		if err != nil {
			return VolumeImage{}, err
		}
		switch part := msg.GetPart().(type) {
		case *pb.BuildVolumeResponse_ImageChunk:
			received += int64(len(part.ImageChunk))
			if received > size.ImageBytes {
				return VolumeImage{}, fmt.Errorf("runner sent more than the %d image bytes it announced", size.ImageBytes)
			}
			hash.Write(part.ImageChunk)
			if _, err := image.Write(part.ImageChunk); err != nil {
				return VolumeImage{}, fmt.Errorf("write image: %w", err)
			}
		case *pb.BuildVolumeResponse_ImageSha256:
			if received != size.ImageBytes {
				return VolumeImage{}, fmt.Errorf("runner sent %d image bytes, announced %d", received, size.ImageBytes)
			}
			digest := hex.EncodeToString(hash.Sum(nil))
			if part.ImageSha256 != digest {
				return VolumeImage{}, fmt.Errorf("image has SHA-256 %s, runner sent %q", digest, part.ImageSha256)
			}
			// The runner's status: a build is only done when the call ends well.
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				if err == nil {
					err = errors.New("runner sent more after the image digest")
				}
				return VolumeImage{}, err
			}
			return VolumeImage{Bytes: received, SHA256: digest}, nil
		default:
			return VolumeImage{}, errors.New("runner sent an answer part this client does not know")
		}
	}
}
