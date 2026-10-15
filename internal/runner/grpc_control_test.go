package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type stopWakeRuntime struct {
	fakeRuntime
	stopCalls int
}

func (s *stopWakeRuntime) StopSandbox(_ context.Context, sandboxID string) error {
	s.stopCalls++
	if sandboxID != "11111111-1111-4111-8111-111111111111" {
		return runnerruntime.ErrSandboxNotFound
	}
	return nil
}

type createOptsRuntime struct {
	fakeRuntime
	opts *runnerruntime.CreateOptions
}

func (c *createOptsRuntime) CreateSandbox(_ context.Context, sandboxID string, opts *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	c.opts = opts
	return &runnerruntime.SandboxInfo{ID: sandboxID, IP: "10.0.0.2"}, nil
}

// create_options_json reaches the runtime as CreateOptions and the response echoes what
// was applied; an empty field is the default policy and an unknown egress is
// refused, not defaulted.
func TestSandboxControlGRPCCreateSandboxParsesOptions(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))

	for _, tc := range []struct {
		name              string
		createOptionsJSON string
		want              runnerruntime.Egress
		wantApplied       string
		wantErr           bool
	}{
		{name: "empty", createOptionsJSON: "", want: runnerruntime.EgressPublic, wantApplied: `{"egress":"public"}`},
		{name: "empty object", createOptionsJSON: "{}", want: runnerruntime.EgressPublic, wantApplied: `{"egress":"public"}`},
		{name: "none", createOptionsJSON: `{"egress":"none"}`, want: runnerruntime.EgressNone, wantApplied: `{"egress":"none"}`},
		{name: "unknown egress", createOptionsJSON: `{"egress":"allow-all"}`, wantErr: true},
		{name: "malformed", createOptionsJSON: `{"egress":`, wantErr: true},
		{name: "null", createOptionsJSON: "null", wantErr: true},
		{name: "not an object", createOptionsJSON: "[]", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &createOptsRuntime{}
			srv := &SandboxControlGRPC{
				Runtime: rt,
				Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
				Rec:     metrics.NewRunnerRecorder(true),
			}
			resp, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: tc.createOptionsJSON})
			if tc.wantErr {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("CreateSandbox() error = %v, want InvalidArgument", err)
				}
				if rt.opts != nil {
					t.Fatal("runtime was called despite invalid options")
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSandbox() failed: %v", err)
			}
			if rt.opts == nil || rt.opts.Egress != tc.want {
				t.Fatalf("runtime options = %+v, want egress %q", rt.opts, tc.want)
			}
			if got := resp.GetAppliedCreateOptionsJson(); got != tc.wantApplied {
				t.Fatalf("applied_create_options_json = %q, want %q", got, tc.wantApplied)
			}
		})
	}
}

type createErrRuntime struct {
	fakeRuntime
	err error
}

func (c *createErrRuntime) CreateSandbox(context.Context, string, *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	return nil, c.err
}

// The API moves a create to another runner only on a capacity refusal, so that
// must arrive marked as one and any other failure must not.
func TestSandboxControlGRPCCreateSandboxErrorCodes(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	for _, tc := range []struct {
		name    string
		err     error
		want    codes.Code
		refusal bool
	}{
		{name: "capacity", err: fmt.Errorf("firecracker %w", runnerruntime.ErrCapacityExhausted), want: codes.ResourceExhausted, refusal: true},
		{name: "other", err: errors.New("clone sandbox rootfs: no space left on device"), want: codes.Internal},
		{name: "deadline", err: context.DeadlineExceeded, want: codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &SandboxControlGRPC{
				Runtime: &createErrRuntime{err: tc.err},
				Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
				Rec:     metrics.NewRunnerRecorder(true),
			}
			_, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: "11111111-1111-4111-8111-111111111111"})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("CreateSandbox() code = %v (%v), want %v", got, err, tc.want)
			}
			if got := runnerctl.IsCapacityRefusal(err); got != tc.refusal {
				t.Fatalf("IsCapacityRefusal(%v) = %v, want %v", err, got, tc.refusal)
			}
		})
	}
}

func TestSandboxControlGRPCStopSandboxRecordsMetric(t *testing.T) {
	rec := metrics.NewRunnerRecorder(true)
	rt := &stopWakeRuntime{}
	srv := &SandboxControlGRPC{
		Runtime: rt,
		Cfg: &config.Config{
			APIKeys: map[string]struct{}{"runner-key": {}},
		},
		Rec: rec,
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	_, err := srv.StopSandbox(ctx, &pb.StopSandboxRequest{SandboxId: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	if rt.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", rt.stopCalls)
	}
	if got := rec.ContainerOpCount(metrics.OpStop, true); got != 1 {
		t.Fatalf("stop metric = %v, want 1", got)
	}
}
