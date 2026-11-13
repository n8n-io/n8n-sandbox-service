package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// buildRuntime is a Firecracker-like runtime: it builds volumes.
type buildRuntime struct {
	fakeRuntime
	// refusal is returned before the tar is read, failure after.
	refusal error
	failure error
	image   []byte
	// announce, if set, is the size passed to deliver instead of the image's.
	announce int64

	calls  int
	params volumebuild.Params
	tar    []byte
}

func (b *buildRuntime) BuildVolume(_ context.Context, params volumebuild.Params, tar io.Reader, deliver func(io.Reader, int64) error) error {
	b.calls++
	b.params = params
	if b.refusal != nil {
		return b.refusal
	}
	data, err := io.ReadAll(tar)
	if err != nil {
		return fmt.Errorf("receive tar: %w", err)
	}
	b.tar = data
	if b.failure != nil {
		return b.failure
	}
	size := int64(len(b.image))
	if b.announce != 0 {
		size = b.announce
	}
	return deliver(bytes.NewReader(b.image), size)
}

// buildStream is the caller's side of a BuildVolume call.
type buildStream struct {
	grpc.ServerStream
	ctx      context.Context
	requests []*pb.BuildVolumeRequest
	// recvErr ends the requests instead of a half-close.
	recvErr error
	// headerDelay is how long the caller takes to send its header.
	headerDelay time.Duration

	received  int
	responses []*pb.BuildVolumeResponse
}

func (s *buildStream) Context() context.Context { return s.ctx }

func (s *buildStream) Recv() (*pb.BuildVolumeRequest, error) {
	if s.received == len(s.requests) {
		if s.recvErr != nil {
			return nil, s.recvErr
		}
		return nil, io.EOF
	}
	if s.received == 0 {
		time.Sleep(s.headerDelay)
	}
	s.received++
	return s.requests[s.received-1], nil
}

func (s *buildStream) Send(response *pb.BuildVolumeResponse) error {
	s.responses = append(s.responses, response)
	return nil
}

func headerOf(header *pb.BuildVolumeHeader) *pb.BuildVolumeRequest {
	return &pb.BuildVolumeRequest{Part: &pb.BuildVolumeRequest_Header{Header: header}}
}

// buildHeader is the header of a tar that decompresses to twice its length.
func buildHeader(tarBytes, entries, contentBytes int64) *pb.BuildVolumeRequest {
	return headerOf(&pb.BuildVolumeHeader{
		TarBytes: tarBytes, Entries: entries, ContentBytes: contentBytes, DecompressedBytes: 2 * tarBytes,
	})
}

func tarChunk(data string) *pb.BuildVolumeRequest {
	return &pb.BuildVolumeRequest{Part: &pb.BuildVolumeRequest_TarChunk{TarChunk: []byte(data)}}
}

func authorized() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
}

func newBuildServer(rt runnerruntime.Runtime) *SandboxControlGRPC {
	return &SandboxControlGRPC{
		Runtime: rt,
		Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:     metrics.NewRunnerRecorder(true),
		Cordon:  cordon.New(),
	}
}

func buildDurations(t *testing.T, srv *SandboxControlGRPC) bool {
	t.Helper()
	body := scrapeRegistry(t, srv.Rec)
	switch {
	case strings.Contains(body, `sandbox_volume_build_duration_seconds_count{role="runner"} 1`):
		return true
	case strings.Contains(body, `sandbox_volume_build_duration_seconds_count{role="runner"} 0`):
		return false
	}
	t.Fatalf("no volume build duration count in:\n%s", body)
	return false
}

func TestBuildVolumeStreamsTheTarInAndTheImageOut(t *testing.T) {
	image := bytes.Repeat([]byte("ext4"), (2*volumeImageChunkBytes+1000)/4)
	rt := &buildRuntime{image: image}
	srv := newBuildServer(rt)
	stream := &buildStream{ctx: authorized(), requests: []*pb.BuildVolumeRequest{
		buildHeader(11, 3, 7), tarChunk("hello "), tarChunk(""), tarChunk("world"),
	}}

	if err := srv.BuildVolume(stream); err != nil {
		t.Fatalf("BuildVolume() failed: %v", err)
	}

	if want := (volumebuild.Params{TarBytes: 11, Entries: 3, ContentBytes: 7, DecompressedBytes: 22}); rt.params != want {
		t.Errorf("the runtime got %+v, want %+v", rt.params, want)
	}
	if string(rt.tar) != "hello world" {
		t.Errorf("the runtime got the tar %q", rt.tar)
	}

	if len(stream.responses) != 5 {
		t.Fatalf("got %d answer parts, want the size, 3 chunks and the digest", len(stream.responses))
	}
	if size, ok := stream.responses[0].GetPart().(*pb.BuildVolumeResponse_ImageBytes); !ok || size.ImageBytes != int64(len(image)) {
		t.Errorf("the answer starts with %v, want the image size %d", stream.responses[0], len(image))
	}
	var got []byte
	for _, response := range stream.responses[1:4] {
		chunk, ok := response.GetPart().(*pb.BuildVolumeResponse_ImageChunk)
		if !ok || len(chunk.ImageChunk) > volumeImageChunkBytes {
			t.Fatalf("answer part is not an image chunk of at most %d bytes", volumeImageChunkBytes)
		}
		got = append(got, chunk.ImageChunk...)
	}
	if !bytes.Equal(got, image) {
		t.Error("the image chunks do not add up to the image")
	}
	digest := sha256.Sum256(image)
	if last := stream.responses[4].GetImageSha256(); last != hex.EncodeToString(digest[:]) {
		t.Errorf("the answer ends with %q, want the image's SHA-256", last)
	}

	if got := srv.Rec.VolumeBuildCount(volumeBuildSuccess); got != 1 {
		t.Errorf("volume_builds_total{result=success} = %v, want 1", got)
	}
	if !buildDurations(t, srv) {
		t.Error("the build's duration was not recorded")
	}
}

// The duration starts at the tar's first byte, as its help says: a slow
// header is not part of the build.
func TestBuildVolumeDurationStartsAtTheTarsFirstByte(t *testing.T) {
	srv := newBuildServer(&buildRuntime{image: []byte("image")})
	stream := &buildStream{ctx: authorized(), headerDelay: 1100 * time.Millisecond, requests: []*pb.BuildVolumeRequest{
		buildHeader(5, 1, 5), tarChunk("hello"),
	}}

	if err := srv.BuildVolume(stream); err != nil {
		t.Fatalf("BuildVolume() failed: %v", err)
	}
	if body := scrapeRegistry(t, srv.Rec); !strings.Contains(body, `sandbox_volume_build_duration_seconds_bucket{role="runner",le="1"} 1`) {
		t.Errorf("the build took a second or more, so its duration counted the header:\n%s", body)
	}
}

func TestBuildVolumeNeedsAVolumeBuilder(t *testing.T) {
	srv := newBuildServer(&fakeRuntime{})
	stream := &buildStream{ctx: authorized(), requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}

	if err := srv.BuildVolume(stream); status.Code(err) != codes.Unimplemented {
		t.Fatalf("BuildVolume() on a runtime that cannot build = %v, want Unimplemented", err)
	}
	if stream.received != 0 {
		t.Errorf("read %d messages", stream.received)
	}
}

func TestBuildVolumeRequiresAPIKey(t *testing.T) {
	rt := &buildRuntime{}
	srv := newBuildServer(rt)
	for name, ctx := range map[string]context.Context{
		"no metadata": context.Background(),
		"wrong key":   metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "other")),
	} {
		stream := &buildStream{ctx: ctx, requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}
		if err := srv.BuildVolume(stream); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: BuildVolume() = %v, want Unauthenticated", name, err)
		}
		if stream.received != 0 || rt.calls != 0 {
			t.Errorf("%s: read %d messages, called the runtime %d times", name, stream.received, rt.calls)
		}
	}
}

func TestBuildVolumeRefusesBeforeReadingAnyTarByte(t *testing.T) {
	diskFull := fmt.Errorf("data volume has 1 MiB free: %w", runnerruntime.ErrCapacityExhausted)
	tests := map[string]struct {
		setup func(srv *SandboxControlGRPC, rt *buildRuntime)
		// wantReceived is how many messages the runner may read: the header at most.
		wantReceived int
		wantResult   string
	}{
		"cordoned": {func(srv *SandboxControlGRPC, _ *buildRuntime) { srv.Cordon.Set(true) }, 0, volumeBuildRefused},
		"not ready": {func(_ *SandboxControlGRPC, rt *buildRuntime) {
			rt.readyErr = errors.New("admission canary has not passed")
		}, 0, volumeBuildRefused},
		"already busy":  {func(_ *SandboxControlGRPC, rt *buildRuntime) { rt.refusal = runnerruntime.ErrVolumeBuildBusy }, 1, volumeBuildBusy},
		"short of disk": {func(_ *SandboxControlGRPC, rt *buildRuntime) { rt.refusal = diskFull }, 1, volumeBuildRefused},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rt := &buildRuntime{}
			srv := newBuildServer(rt)
			tt.setup(srv, rt)
			stream := &buildStream{ctx: authorized(), requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}

			err := srv.BuildVolume(stream)

			if !runnerctl.IsCapacityRefusal(err) {
				t.Fatalf("BuildVolume() = %v, want a capacity refusal", err)
			}
			if stream.received != tt.wantReceived {
				t.Errorf("read %d messages, want %d", stream.received, tt.wantReceived)
			}
			if len(stream.responses) != 0 {
				t.Errorf("sent %d answer parts", len(stream.responses))
			}
			if got := srv.Rec.VolumeBuildCount(tt.wantResult); got != 1 {
				t.Errorf("volume_builds_total{result=%s} = %v, want 1", tt.wantResult, got)
			}
			if buildDurations(t, srv) {
				t.Error("a refused build's duration was recorded")
			}
		})
	}
}

func TestBuildVolumeHoldsTheCallerToTheProtocol(t *testing.T) {
	tests := map[string]struct {
		requests []*pb.BuildVolumeRequest
		// wantCalls is whether the runtime was asked to build at all.
		wantCalls int
	}{
		"no header":                    {nil, 0},
		"a tar chunk first":            {[]*pb.BuildVolumeRequest{tarChunk("hello")}, 0},
		"an empty first message":       {[]*pb.BuildVolumeRequest{{}}, 0},
		"a header without tar bytes":   {[]*pb.BuildVolumeRequest{buildHeader(0, 1, 0)}, 0},
		"a header without entries":     {[]*pb.BuildVolumeRequest{buildHeader(5, 0, 5), tarChunk("hello")}, 0},
		"a negative content size":      {[]*pb.BuildVolumeRequest{buildHeader(5, 1, -1), tarChunk("hello")}, 0},
		"a header beyond the bounds":   {[]*pb.BuildVolumeRequest{buildHeader(1<<40+1, 1, 0)}, 0},
		"no decompressed length":       {[]*pb.BuildVolumeRequest{headerOf(&pb.BuildVolumeHeader{TarBytes: 5, Entries: 1, ContentBytes: 5}), tarChunk("hello")}, 0},
		"a second header":              {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("he"), buildHeader(5, 1, 5)}, 1},
		"an empty message in the tar":  {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("he"), {}}, 1},
		"fewer bytes than the header":  {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hell")}, 1},
		"no bytes at all":              {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5)}, 1},
		"more bytes than the header":   {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello"), tarChunk("!")}, 1},
		"a chunk that crosses the end": {[]*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello!")}, 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rt := &buildRuntime{image: []byte("image")}
			srv := newBuildServer(rt)
			stream := &buildStream{ctx: authorized(), requests: tt.requests}

			err := srv.BuildVolume(stream)

			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("BuildVolume() = %v, want InvalidArgument", err)
			}
			if _, marked := runnerctl.BuildFailureReason(err); marked || runnerctl.IsCapacityRefusal(err) {
				t.Errorf("BuildVolume() = %v: a broken call is neither a failed build nor a refusal", err)
			}
			if rt.calls != tt.wantCalls {
				t.Errorf("the runtime was called %d times, want %d", rt.calls, tt.wantCalls)
			}
			if len(stream.responses) != 0 {
				t.Errorf("sent %d answer parts", len(stream.responses))
			}
			if got := srv.Rec.VolumeBuildCount(volumeBuildInvalidRequest); got != 1 {
				t.Errorf("volume_builds_total{result=invalid_request} = %v, want 1", got)
			}
		})
	}
}

// A tar is only complete once the caller half-closes: a call that breaks after
// the last byte is not a build.
func TestBuildVolumeWaitsForTheHalfClose(t *testing.T) {
	rt := &buildRuntime{image: []byte("image")}
	srv := newBuildServer(rt)
	stream := &buildStream{
		ctx:      authorized(),
		requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")},
		recvErr:  status.Error(codes.Canceled, "context canceled"),
	}

	if err := srv.BuildVolume(stream); status.Code(err) != codes.Canceled {
		t.Fatalf("BuildVolume() = %v, want Canceled", err)
	}
	if len(stream.responses) != 0 || rt.tar != nil {
		t.Errorf("built from a tar the caller never finished: %d answer parts", len(stream.responses))
	}
	if got := srv.Rec.VolumeBuildCount(volumeBuildCanceled); got != 1 {
		t.Errorf("volume_builds_total{result=canceled} = %v, want 1", got)
	}
}

func TestBuildVolumeTellsABadTarFromARunnerFault(t *testing.T) {
	const runtimeText = "/srv/firecracker/data/volume-build/tenant-file"
	failed := func(reason string) error {
		return &runnerruntime.VolumeBuildError{Reason: reason, Err: errors.New("entry \"" + runtimeText + "\": refused")}
	}
	tests := map[string]struct {
		failure    error
		wantCode   codes.Code
		wantReason string
	}{
		"tar":       {failed(volumebuild.ReasonTar), codes.InvalidArgument, volumebuild.ReasonTar},
		"too large": {failed(volumebuild.ReasonTooLarge), codes.InvalidArgument, volumebuild.ReasonTooLarge},
		"no space":  {failed(volumebuild.ReasonNoSpace), codes.InvalidArgument, volumebuild.ReasonNoSpace},
		"format":    {failed(volumebuild.ReasonFormat), codes.Internal, volumebuild.ReasonFormat},
		"mount":     {failed(volumebuild.ReasonMount), codes.Internal, volumebuild.ReasonMount},
		"fsck":      {failed(volumebuild.ReasonFsck), codes.Internal, volumebuild.ReasonFsck},
		"internal":  {failed(volumebuild.ReasonInternal), codes.Internal, volumebuild.ReasonInternal},
		"timeout":   {failed(runnerruntime.VolumeBuildTimeout), codes.DeadlineExceeded, runnerruntime.VolumeBuildTimeout},
		// Only the fixed reasons leave the runner, as label or to the API.
		"a reason not on the list": {failed("tar\",result=\"success"), codes.Internal, volumebuild.ReasonInternal},
		"a runner fault":           {errors.New("prepare volume build: mount " + runtimeText + " failed"), codes.Internal, volumebuild.ReasonInternal},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rt := &buildRuntime{failure: tt.failure}
			srv := newBuildServer(rt)
			stream := &buildStream{ctx: authorized(), requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}

			err := srv.BuildVolume(stream)

			if status.Code(err) != tt.wantCode {
				t.Errorf("BuildVolume() = %v, want code %s", err, tt.wantCode)
			}
			if reason, ok := runnerctl.BuildFailureReason(err); !ok || reason != tt.wantReason {
				t.Errorf("BuildFailureReason() = %q, %v; want %q", reason, ok, tt.wantReason)
			}
			if runnerctl.IsCapacityRefusal(err) {
				t.Error("a failed build must not look like a refusal: the API would retry it elsewhere")
			}
			if strings.Contains(err.Error(), runtimeText) {
				t.Errorf("the error carries the runtime's text: %v", err)
			}
			if got := srv.Rec.VolumeBuildCount(tt.wantReason); got != 1 {
				t.Errorf("volume_builds_total{result=%s} = %v, want 1", tt.wantReason, got)
			}
			if !buildDurations(t, srv) {
				t.Error("the failed build's duration was not recorded")
			}
		})
	}
}

func TestBuildVolumeCountsACallerThatWentAway(t *testing.T) {
	ctx, cancel := context.WithCancel(authorized())
	cancel()
	rt := &buildRuntime{failure: context.Canceled}
	srv := newBuildServer(rt)
	stream := &buildStream{ctx: ctx, requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}

	err := srv.BuildVolume(stream)

	if status.Code(err) != codes.Canceled {
		t.Fatalf("BuildVolume() = %v, want Canceled", err)
	}
	if _, marked := runnerctl.BuildFailureReason(err); marked {
		t.Error("a canceled call is not a failed build")
	}
	if got := srv.Rec.VolumeBuildCount(volumeBuildCanceled); got != 1 {
		t.Errorf("volume_builds_total{result=canceled} = %v, want 1", got)
	}
}

func TestBuildVolumeFailsOnAnImageOfAnotherSize(t *testing.T) {
	rt := &buildRuntime{image: []byte("image"), announce: 6}
	srv := newBuildServer(rt)
	stream := &buildStream{ctx: authorized(), requests: []*pb.BuildVolumeRequest{buildHeader(5, 1, 5), tarChunk("hello")}}

	err := srv.BuildVolume(stream)

	if reason, ok := runnerctl.BuildFailureReason(err); !ok || reason != volumebuild.ReasonInternal {
		t.Fatalf("BuildVolume() = %v, want a failed build with reason internal", err)
	}
	for _, response := range stream.responses {
		if response.GetImageSha256() != "" {
			t.Error("sent a digest for an image that is not the announced one")
		}
	}
}

func TestBuildVolumeOverControlGRPC(t *testing.T) {
	pki := newTestPKI(t)
	tlsCfg := &runnerctl.TLS{CAFile: pki.caFile, CertFile: pki.clientCertFile, KeyFile: pki.clientKeyFile, ServerName: "localhost"}
	tar := bytes.Repeat([]byte("tar bytes "), 250000)
	header := &pb.BuildVolumeHeader{TarBytes: int64(len(tar)), Entries: 12, ContentBytes: 34, DecompressedBytes: int64(len(tar))}
	image := bytes.Repeat([]byte("image bytes "), 300000)
	digest := sha256.Sum256(image)
	ctx := context.Background()

	t.Run("a build", func(t *testing.T) {
		rt := &buildRuntime{image: image}
		addr := serveControlGRPCWith(t, pki, cordon.New(), rt)
		var got bytes.Buffer

		result, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, bytes.NewReader(tar), &got)

		if err != nil {
			t.Fatalf("BuildVolume() failed: %v", err)
		}
		if want := (runnerctl.VolumeImage{Bytes: int64(len(image)), SHA256: hex.EncodeToString(digest[:])}); result != want {
			t.Errorf("BuildVolume() = %+v, want %+v", result, want)
		}
		if !bytes.Equal(got.Bytes(), image) {
			t.Error("the image written is not the image built")
		}
		if !bytes.Equal(rt.tar, tar) {
			t.Error("the tar built from is not the tar sent")
		}
	})

	t.Run("a refusal", func(t *testing.T) {
		addr := serveControlGRPCWith(t, pki, cordon.New(), &buildRuntime{refusal: runnerruntime.ErrVolumeBuildBusy})
		source := &slowReader{reader: bytes.NewReader(tar)}
		_, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, source, io.Discard)
		source.released.Store(true)
		if !runnerctl.IsCapacityRefusal(err) {
			t.Fatalf("BuildVolume() = %v, want a capacity refusal", err)
		}
		// The API rewinds the tar for the next runner: nothing may read it now.
		time.Sleep(4 * slowReaderDelay)
		if source.readLate.Load() {
			t.Error("BuildVolume() still read the tar after it returned")
		}
	})

	t.Run("a failed build", func(t *testing.T) {
		failure := &runnerruntime.VolumeBuildError{Reason: volumebuild.ReasonTar, Err: errors.New("hard link")}
		addr := serveControlGRPCWith(t, pki, cordon.New(), &buildRuntime{failure: failure})
		_, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, bytes.NewReader(tar), io.Discard)
		if reason, ok := runnerctl.BuildFailureReason(err); !ok || reason != volumebuild.ReasonTar {
			t.Fatalf("BuildVolume() = %v, want a failed build with reason tar", err)
		}
	})

	t.Run("a runner that cannot build", func(t *testing.T) {
		addr := serveControlGRPCWith(t, pki, cordon.New(), &fakeRuntime{})
		_, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, bytes.NewReader(tar), io.Discard)
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("BuildVolume() = %v, want Unimplemented", err)
		}
	})

	t.Run("a wrong key", func(t *testing.T) {
		rt := &buildRuntime{image: image}
		addr := serveControlGRPCWith(t, pki, cordon.New(), rt)
		_, err := runnerctl.BuildVolume(ctx, addr, "wrong", tlsCfg, header, bytes.NewReader(tar), io.Discard)
		if status.Code(err) != codes.Unauthenticated || rt.calls != 0 {
			t.Fatalf("BuildVolume() = %v after %d runtime calls, want Unauthenticated and none", err, rt.calls)
		}
	})

	t.Run("a tar shorter than its header", func(t *testing.T) {
		addr := serveControlGRPCWith(t, pki, cordon.New(), &buildRuntime{image: image})
		_, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, bytes.NewReader(tar[:len(tar)-1]), io.Discard)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("BuildVolume() = %v, want InvalidArgument", err)
		}
	})

	t.Run("a tar that cannot be read", func(t *testing.T) {
		addr := serveControlGRPCWith(t, pki, cordon.New(), &buildRuntime{image: image})
		broken := errors.New("disk error")
		_, err := runnerctl.BuildVolume(ctx, addr, "secret", tlsCfg, header, io.MultiReader(bytes.NewReader(tar[:100]), iotest.ErrReader(broken)), io.Discard)
		if !errors.Is(err, broken) {
			t.Fatalf("BuildVolume() = %v, want the read error", err)
		}
	})
}

const slowReaderDelay = 25 * time.Millisecond

// slowReader is a tar that reads slower than a runner refuses, and notices a
// read after its caller got it back.
type slowReader struct {
	reader   io.Reader
	released atomic.Bool
	readLate atomic.Bool
}

func (s *slowReader) Read(buf []byte) (int, error) {
	// A read that began before the caller got the tar back still moves it.
	time.Sleep(slowReaderDelay)
	if s.released.Load() {
		s.readLate.Store(true)
	}
	return s.reader.Read(buf)
}

func scrapeRegistry(t *testing.T, rec *metrics.RunnerRecorder) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler(rec.Registry()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", recorder.Code)
	}
	return recorder.Body.String()
}
