package runner

import (
	"context"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"google.golang.org/grpc/metadata"
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
