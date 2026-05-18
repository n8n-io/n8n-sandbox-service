package manager

import (
	"errors"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

func TestDefaultLimitsEmitsDiskQuotaIndependentlyOfCgroups(t *testing.T) {
	// cgroup-based limits (memory/cpu/pids) and the disk quota are enforced
	// via different mechanisms; disabling cgroups must not also drop the
	// disk quota flag.
	m := &Manager{config: &config.Config{
		EnableCgroups:      false,
		DefaultMemoryMB:    512,
		DefaultCPUPercent:  100,
		DefaultPidsMax:     128,
		DiskQuotaActive:    true,
		DefaultDiskQuotaMB: 1024,
	}}

	limits := m.defaultLimits()

	if limits.MemoryMB != 0 {
		t.Errorf("expected MemoryMB 0 when EnableCgroups=false, got %d", limits.MemoryMB)
	}
	if limits.CPUPercent != 0 {
		t.Errorf("expected CPUPercent 0 when EnableCgroups=false, got %d", limits.CPUPercent)
	}
	if limits.PidsMax != 0 {
		t.Errorf("expected PidsMax 0 when EnableCgroups=false, got %d", limits.PidsMax)
	}
	if limits.DiskMB != 1024 {
		t.Errorf("expected DiskMB 1024 when DiskQuotaActive=true regardless of cgroups, got %d", limits.DiskMB)
	}
}

func TestDefaultLimitsOmitsDiskWhenQuotaInactive(t *testing.T) {
	m := &Manager{config: &config.Config{
		EnableCgroups:      true,
		DefaultMemoryMB:    512,
		DefaultCPUPercent:  100,
		DefaultPidsMax:     128,
		DiskQuotaActive:    false,
		DefaultDiskQuotaMB: 1024,
	}}

	limits := m.defaultLimits()

	if limits.DiskMB != 0 {
		t.Errorf("expected DiskMB 0 when DiskQuotaActive=false, got %d", limits.DiskMB)
	}
	if limits.MemoryMB != 512 {
		t.Errorf("expected MemoryMB 512 when EnableCgroups=true, got %d", limits.MemoryMB)
	}
}

func TestDockerLimitArgs(t *testing.T) {
	limits := &ResourceLimits{
		MemoryMB:   512,
		CPUPercent: 150,
		PidsMax:    128,
	}

	got := dockerLimitArgs(limits)
	want := []string{"--memory", "512m", "--cpus", "1.50", "--pids-limit", "128"}
	if len(got) != len(want) {
		t.Fatalf("dockerLimitArgs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dockerLimitArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDockerLimitArgsIncludesDiskWhenSet(t *testing.T) {
	limits := &ResourceLimits{
		MemoryMB:   512,
		CPUPercent: 100,
		PidsMax:    128,
		DiskMB:     1024,
	}

	got := dockerLimitArgs(limits)
	want := []string{"--memory", "512m", "--cpus", "1.00", "--pids-limit", "128", "--storage-opt", "size=1024m"}
	if len(got) != len(want) {
		t.Fatalf("dockerLimitArgs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dockerLimitArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDockerLimitArgsOmitsDiskWhenZero(t *testing.T) {
	limits := &ResourceLimits{
		MemoryMB:   512,
		CPUPercent: 100,
		PidsMax:    128,
		DiskMB:     0,
	}

	got := dockerLimitArgs(limits)
	for _, arg := range got {
		if arg == "--storage-opt" {
			t.Fatalf("dockerLimitArgs() unexpectedly included --storage-opt: %#v", got)
		}
	}
}

func TestIsDockerNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "container not found",
			err:  errors.New("Error response from daemon: No such container: sandbox-1"),
			want: true,
		},
		{
			name: "network not found",
			err:  errors.New("Error response from daemon: No such network: runner-bridge"),
			want: true,
		},
		{
			name: "image not found",
			err:  errors.New("Error response from daemon: No such image: sandbox-custom-1"),
			want: true,
		},
		{
			name: "generic error",
			err:  errors.New("Cannot connect to the Docker daemon"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isDockerNotFound(tc.err); got != tc.want {
				t.Fatalf("isDockerNotFound() = %v, want %v", got, tc.want)
			}
		})
	}
}
