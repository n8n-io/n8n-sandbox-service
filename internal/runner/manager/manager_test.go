package manager

import (
	"errors"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

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

func TestDockerDiskQuotaArgs(t *testing.T) {
	tests := []struct {
		name   string
		limits *ResourceLimits
		want   []string
	}{
		{name: "set", limits: &ResourceLimits{DiskMB: 1024}, want: []string{"--storage-opt", "size=1024m"}},
		{name: "zero", limits: &ResourceLimits{DiskMB: 0}, want: nil},
		{name: "nil", limits: nil, want: nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := dockerDiskQuotaArgs(tc.limits)
			if len(got) != len(tc.want) {
				t.Fatalf("dockerDiskQuotaArgs() = %#v, want %#v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("dockerDiskQuotaArgs()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDefaultLimitsAppliesDiskQuotaOnlyWhenActive(t *testing.T) {
	// xfs project quotas only enforce when the runner's storage pool was set
	// up successfully (signaled by DiskQuotaActive). Otherwise we must not
	// emit `--storage-opt size=`, regardless of the configured default.
	tests := []struct {
		name   string
		active bool
		want   int64
	}{
		{name: "active", active: true, want: 1024},
		{name: "inactive", active: false, want: 0},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{config: &config.Config{
				DiskQuotaActive:    tc.active,
				DefaultDiskQuotaMB: 1024,
			}}
			if got := m.defaultLimits().DiskMB; got != tc.want {
				t.Errorf("defaultLimits().DiskMB = %d, want %d", got, tc.want)
			}
		})
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
