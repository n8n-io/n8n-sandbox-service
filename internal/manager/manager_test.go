package manager

import (
	"errors"
	"strings"
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

func TestBuildDockerfile(t *testing.T) {
	dockerfile, err := buildDockerfile("sandbox-base:latest", []string{"RUN apt-get update", "RUN apt-get install -y git"})
	if err != nil {
		t.Fatalf("buildDockerfile() error = %v", err)
	}

	for _, want := range []string{
		"FROM sandbox-base:latest",
		"RUN apt-get update",
		"RUN apt-get install -y git",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
	if strings.Contains(dockerfile, "USER root") {
		t.Fatalf("dockerfile should not inject USER root:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "USER 1000:1000") {
		t.Fatalf("dockerfile should not inject USER 1000:1000:\n%s", dockerfile)
	}
}

func TestBuildDockerfileWithNonRunSteps(t *testing.T) {
	dockerfile, err := buildDockerfile("sandbox-base:latest", []string{"COPY foo /bar", "ENV MY_VAR=1", "RUN echo hello"})
	if err != nil {
		t.Fatalf("buildDockerfile() error = %v", err)
	}

	for _, want := range []string{
		"COPY foo /bar",
		"ENV MY_VAR=1",
		"RUN echo hello",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
}

func TestBuildDockerfileAllowsExplicitUserRoot(t *testing.T) {
	dockerfile, err := buildDockerfile("sandbox-base:latest", []string{"USER root", "RUN apt-get update"})
	if err != nil {
		t.Fatalf("buildDockerfile() error = %v", err)
	}

	for _, want := range []string{
		"FROM sandbox-base:latest",
		"USER root",
		"RUN apt-get update",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
}

func TestBuildDockerfileAllowsExplicitNonRootUser(t *testing.T) {
	dockerfile, err := buildDockerfile("sandbox-base:latest", []string{"USER 1000:1000", "RUN whoami"})
	if err != nil {
		t.Fatalf("buildDockerfile() error = %v", err)
	}

	for _, want := range []string{
		"FROM sandbox-base:latest",
		"USER 1000:1000",
		"RUN whoami",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
}

func TestStepsHash(t *testing.T) {
	h1 := stepsHash("base:latest", []string{"RUN a", "RUN b"})
	h2 := stepsHash("base:latest", []string{"RUN a", "RUN b"})
	if h1 != h2 {
		t.Fatalf("same inputs should produce same hash: %q != %q", h1, h2)
	}

	h3 := stepsHash("base:latest", []string{"RUN b", "RUN a"})
	if h1 == h3 {
		t.Fatal("different step order should produce different hash")
	}

	h4 := stepsHash("other:latest", []string{"RUN a", "RUN b"})
	if h1 == h4 {
		t.Fatal("different base image should produce different hash")
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

func TestShouldDeleteCachedImageRecord(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "transient docker error",
			err:  errors.New("Cannot connect to the Docker daemon"),
			want: false,
		},
		{
			name: "missing image",
			err:  errors.New("Error response from daemon: No such image: sandbox-custom-1"),
			want: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldDeleteCachedImageRecord(tc.err); got != tc.want {
				t.Fatalf("shouldDeleteCachedImageRecord() = %v, want %v", got, tc.want)
			}
		})
	}
}
