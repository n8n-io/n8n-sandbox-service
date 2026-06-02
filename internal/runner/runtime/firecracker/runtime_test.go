package firecracker

import (
	"context"
	"errors"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func TestStubRuntimeReportsNotReady(t *testing.T) {
	rt := New(&config.Config{CapacityTotal: 12})

	if err := rt.Ready(context.Background()); err == nil {
		t.Fatal("expected Firecracker stub to report not ready")
	}

	capacity, err := rt.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() failed: %v", err)
	}
	if capacity.Used != 0 || capacity.Total != 12 {
		t.Fatalf("Capacity() = %+v, want used=0 total=12", capacity)
	}
}

func TestStubRuntimeTreatsSandboxesAsMissing(t *testing.T) {
	rt := New(&config.Config{})

	_, err := rt.GetSandboxInfo(context.Background(), "sandbox-id")
	if !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want ErrSandboxNotFound", err)
	}

	if err := rt.DeleteSandbox(context.Background(), "sandbox-id"); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("DeleteSandbox() error = %v, want ErrSandboxNotFound", err)
	}
}
