package main

import (
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	firecrackerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker"
)

func TestNewRuntimeSelectsFirecracker(t *testing.T) {
	rt, err := newRuntime(&config.Config{Backend: config.BackendFirecracker})
	if err != nil {
		t.Fatalf("newRuntime() failed: %v", err)
	}
	if _, ok := rt.(*firecrackerruntime.Runtime); !ok {
		t.Fatalf("newRuntime() returned %T, want *firecrackerruntime.Runtime", rt)
	}
}

func TestNewRuntimeRejectsUnsupportedBackend(t *testing.T) {
	if _, err := newRuntime(&config.Config{Backend: config.Backend("unknown")}); err == nil {
		t.Fatal("expected newRuntime to reject unsupported backend")
	}
}
