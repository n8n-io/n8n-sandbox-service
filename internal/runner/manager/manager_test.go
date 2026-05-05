package manager

import (
	"errors"
	"testing"
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
