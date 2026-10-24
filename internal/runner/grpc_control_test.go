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
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
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
// refused, not defaulted. Templates other than the default and egress proxy,
// which no runtime serves yet, are refused before the runtime is called.
func TestSandboxControlGRPCCreateSandboxParsesOptions(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))

	for _, tc := range []struct {
		name              string
		createOptionsJSON string
		want              runnerruntime.Egress
		wantApplied       string
		wantErr           codes.Code
	}{
		{name: "empty", createOptionsJSON: "", want: runnerruntime.EgressPublic, wantApplied: `{"egress":"public"}`},
		{name: "empty object", createOptionsJSON: "{}", want: runnerruntime.EgressPublic, wantApplied: `{"egress":"public"}`},
		{name: "none", createOptionsJSON: `{"egress":"none"}`, want: runnerruntime.EgressNone, wantApplied: `{"egress":"none"}`},
		{name: "default template", createOptionsJSON: `{"egress":"none","template":"default"}`, want: runnerruntime.EgressNone, wantApplied: `{"egress":"none"}`},
		{name: "unknown egress", createOptionsJSON: `{"egress":"allow-all"}`, wantErr: codes.InvalidArgument},
		{name: "malformed", createOptionsJSON: `{"egress":`, wantErr: codes.InvalidArgument},
		{name: "null", createOptionsJSON: "null", wantErr: codes.InvalidArgument},
		{name: "not an object", createOptionsJSON: "[]", wantErr: codes.InvalidArgument},
		// Refused like a full runner, so the API tries another one.
		{name: "other template", createOptionsJSON: `{"template":"python"}`, wantErr: codes.ResourceExhausted},
		{name: "invalid template", createOptionsJSON: `{"template":"../x"}`, wantErr: codes.InvalidArgument},
		{name: "proxy", createOptionsJSON: `{"egress":"proxy","tenant":"t1","egress_proxy":{"allow":[{"name":"a","hosts":["example.com"]}]}}`, wantErr: codes.Unimplemented},
		{name: "proxy without spec", createOptionsJSON: `{"egress":"proxy"}`, wantErr: codes.InvalidArgument},
		{name: "spec without proxy", createOptionsJSON: `{"egress":"none","egress_proxy":{"allow":[]}}`, wantErr: codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &createOptsRuntime{}
			srv := &SandboxControlGRPC{
				Runtime: rt,
				Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
				Rec:     metrics.NewRunnerRecorder(true),
				Cordon:  cordon.New(),
			}
			resp, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: tc.createOptionsJSON})
			if tc.wantErr != codes.OK {
				if status.Code(err) != tc.wantErr {
					t.Fatalf("CreateSandbox() error = %v, want %v", err, tc.wantErr)
				}
				if tc.wantErr == codes.ResourceExhausted && !runnerctl.IsCapacityRefusal(err) {
					t.Fatalf("CreateSandbox() error = %v, want a capacity refusal", err)
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
				Cordon:  cordon.New(),
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

// A cordoned runner refuses creates as a capacity refusal, before the runtime is
// asked, so the API retries on another runner; uncordoning lets them through.
func TestSandboxControlGRPCCreateSandboxRefusedWhileCordoned(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	rt := &createOptsRuntime{}
	cd := cordon.New()
	srv := &SandboxControlGRPC{
		Runtime: rt,
		Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:     metrics.NewRunnerRecorder(true),
		Cordon:  cd,
	}

	cd.Set(true)
	_, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id})
	if !runnerctl.IsCapacityRefusal(err) {
		t.Fatalf("CreateSandbox() while cordoned = %v, want a capacity refusal", err)
	}
	if rt.opts != nil {
		t.Fatal("runtime was asked to create while cordoned")
	}

	cd.Set(false)
	if _, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("CreateSandbox() after uncordon failed: %v", err)
	}
}

func TestSandboxControlGRPCCordon(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	cd := cordon.New()
	srv := &SandboxControlGRPC{
		Runtime: &fakeRuntime{},
		Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:     metrics.NewRunnerRecorder(false),
		Cordon:  cd,
	}

	for _, step := range []struct {
		set  *bool
		want bool
	}{
		{nil, false},
		{ptr(true), true},
		{ptr(true), true},
		{nil, true},
		{ptr(false), false},
		{ptr(false), false},
		{nil, false},
	} {
		var got bool
		if step.set == nil {
			resp, err := srv.GetCordon(ctx, &pb.GetCordonRequest{})
			if err != nil {
				t.Fatalf("GetCordon() failed: %v", err)
			}
			got = resp.GetCordoned()
		} else {
			resp, err := srv.SetCordon(ctx, &pb.SetCordonRequest{Cordoned: *step.set})
			if err != nil {
				t.Fatalf("SetCordon(%v) failed: %v", *step.set, err)
			}
			got = resp.GetCordoned()
		}
		if got != step.want || cd.Cordoned() != step.want {
			t.Fatalf("cordoned: response %v, state %v, want %v", got, cd.Cordoned(), step.want)
		}
	}
}

// Cordoning stops placement on the runner, so it takes the same API key as the
// lifecycle calls; the listener has already required a client certificate.
func TestSandboxControlGRPCCordonRequiresAPIKey(t *testing.T) {
	cd := cordon.New()
	srv := &SandboxControlGRPC{
		Runtime: &fakeRuntime{},
		Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:     metrics.NewRunnerRecorder(false),
		Cordon:  cd,
	}
	for name, ctx := range map[string]context.Context{
		"no metadata": context.Background(),
		"no key":      metadata.NewIncomingContext(context.Background(), metadata.Pairs()),
		"wrong key":   metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "wrong")),
	} {
		if _, err := srv.SetCordon(ctx, &pb.SetCordonRequest{Cordoned: true}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("SetCordon with %s: error = %v, want Unauthenticated", name, err)
		}
		if _, err := srv.GetCordon(ctx, &pb.GetCordonRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("GetCordon with %s: error = %v, want Unauthenticated", name, err)
		}
	}
	if cd.Cordoned() {
		t.Fatal("an unauthenticated call cordoned the runner")
	}
}

func ptr[T any](v T) *T { return &v }

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

// A runtime that serves a template gets creates for it; one it does not serve
// is still refused.
func TestSandboxControlGRPCCreateSandboxServedTemplate(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	rt := &createOptsRuntime{fakeRuntime: fakeRuntime{templates: []runnerruntime.TemplateInfo{{Name: "default"}, {Name: "python"}}}}
	srv := &SandboxControlGRPC{Runtime: rt, Cfg: &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}}, Rec: metrics.NewRunnerRecorder(false), Cordon: cordon.New()}
	resp, err := srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: "11111111-1111-4111-8111-111111111111", CreateOptionsJson: `{"template":"python"}`})
	if err != nil || rt.opts.Template != "python" || resp.GetAppliedCreateOptionsJson() != `{"egress":"public","template":"python"}` {
		t.Fatalf("python: resp=%v err=%v opts=%+v", resp, err, rt.opts)
	}
	_, err = srv.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: "11111111-1111-4111-8111-111111111111", CreateOptionsJson: `{"template":"go"}`})
	if !runnerctl.IsCapacityRefusal(err) {
		t.Fatalf("go: %v, want a capacity refusal", err)
	}
}
