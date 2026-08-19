package runner

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

type traceCapturingRuntime struct {
	fakeRuntime
	traceID string
}

func (r *traceCapturingRuntime) StopSandbox(ctx context.Context, _ string) error {
	r.traceID = obs.TraceID(ctx)
	return nil
}

func stopWithMetadata(t *testing.T, pairs ...string) string {
	t.Helper()
	rt := &traceCapturingRuntime{}
	srv := &SandboxControlGRPC{
		Runtime: rt,
		Cfg:     &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:     metrics.NewRunnerRecorder(false),
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
	if _, err := srv.StopSandbox(ctx, &pb.StopSandboxRequest{SandboxId: "11111111-1111-4111-8111-111111111111"}); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	return rt.traceID
}

func TestSandboxControlGRPCAdoptsCallerTrace(t *testing.T) {
	got := stopWithMetadata(t,
		"x-api-key", "runner-key",
		obs.HeaderTraceparent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	)
	if got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("runtime trace id = %q, want the caller's trace id", got)
	}
}

func TestSandboxControlGRPCMintsTraceWhenCallerSendsNone(t *testing.T) {
	if got := stopWithMetadata(t, "x-api-key", "runner-key"); len(got) != 32 {
		t.Fatalf("runtime trace id = %q, want a freshly minted id", got)
	}
}

func TestSandboxControlGRPCReplacesMalformedTrace(t *testing.T) {
	got := stopWithMetadata(t,
		"x-api-key", "runner-key",
		obs.HeaderTraceparent, "not-a-traceparent",
	)
	if len(got) != 32 {
		t.Fatalf("runtime trace id = %q, want a freshly minted id", got)
	}
}
