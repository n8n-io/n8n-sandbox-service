package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

type fakeDockerBackend struct {
	events         *[]string
	containerID    string
	ip             string
	containerIPErr error
	stopErr        error
}

func (f *fakeDockerBackend) ping(context.Context) error {
	return errors.New("unexpected ping")
}

func (f *fakeDockerBackend) createContainer(context.Context, string, string, string, *ResourceLimits, bool) (string, error) {
	return "", errors.New("unexpected createContainer")
}

func (f *fakeDockerBackend) startContainer(context.Context, string) error {
	*f.events = append(*f.events, "start")
	return nil
}

func (f *fakeDockerBackend) stopContainer(context.Context, string) error {
	*f.events = append(*f.events, "stop")
	return f.stopErr
}

func (f *fakeDockerBackend) removeContainer(context.Context, string) error {
	return errors.New("unexpected removeContainer")
}

func (f *fakeDockerBackend) containerIP(context.Context, string) (string, error) {
	*f.events = append(*f.events, "containerIP")
	if f.containerIPErr != nil {
		return "", f.containerIPErr
	}
	return f.ip, nil
}

func (f *fakeDockerBackend) inspectContainer(context.Context, string) (*containerInspect, error) {
	*f.events = append(*f.events, "inspect")
	return &containerInspect{
		ID:    f.containerID,
		State: containerState{Status: containerStatusExited},
	}, nil
}

func (f *fakeDockerBackend) inspectNetwork(context.Context, string) (*networkInspect, error) {
	return nil, errors.New("unexpected inspectNetwork")
}

func (f *fakeDockerBackend) findContainerByLabels(context.Context, ...string) ([]string, error) {
	*f.events = append(*f.events, "find")
	return []string{f.containerID}, nil
}

func (f *fakeDockerBackend) pullImage(context.Context, string) error {
	return errors.New("unexpected pullImage")
}

func (f *fakeDockerBackend) watchContainerDeaths(context.Context, func(string, string)) error {
	return errors.New("unexpected watchContainerDeaths")
}

func (f *fakeDockerBackend) run(context.Context, ...string) (string, error) {
	return "", errors.New("unexpected run")
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

// Every container ID the runner holds is matched against the ID a die event
// carries, and events carry the full 64-character one. docker ps abbreviates to 12
// unless told not to, and an abbreviated ID in the expected-stop map means a stop
// the runner asked for never matches the death it caused: the stop is read as a
// crash, and the next request for an idle-stopped sandbox is refused with a 409 it
// should never see.
func TestContainerIDArgsAskDockerForUntruncatedIDs(t *testing.T) {
	got := containerIDArgs("label=managed=true", "label=sandbox=abc")
	want := []string{"ps", "-aq", "--no-trunc", "--filter", "label=managed=true", "--filter", "label=sandbox=abc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containerIDArgs() = %#v, want %#v", got, want)
	}
}

func TestDockerContainerCreateArgs(t *testing.T) {
	limits := &ResourceLimits{
		MemoryMB:   512,
		CPUPercent: 150,
		PidsMax:    128,
		DiskMB:     1024,
	}

	got := dockerContainerCreateArgs("sandbox-id", "sandbox-name", "sandbox-image", limits, true)
	want := []string{
		"container", "create",
		"--name", "sandbox-name",
		"--hostname", "sandbox",
		"--restart", "unless-stopped",
		"--network", runnerBridgeNetwork,
		"--label", containerLabelManaged + "=" + containerLabelManagedVal,
		"--label", containerLabelSandboxID + "=sandbox-id",
		"--user", "1000:1000",
		"--env", "HOME=/home/user",
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.lo.disable_ipv6=1",
		"--memory", "512m",
		"--cpus", "1.50",
		"--pids-limit", "128",
		"--storage-opt", "size=1024m",
		"sandbox-image",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dockerContainerCreateArgs() = %#v, want %#v", got, want)
	}
}

// Comparing every extracted flag against the whole policy is what catches a
// --cap-add creeping back in: it shows up as an extra pair.
func TestDockerContainerCreateArgsAlwaysApplySecurityPolicy(t *testing.T) {
	want := []string{
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	}

	for _, enableCgroups := range []bool{false, true} {
		gotArgs := dockerContainerCreateArgs("sandbox-id", "sandbox-name", "sandbox-image", nil, enableCgroups)
		var got []string
		for i := 0; i < len(gotArgs); i++ {
			if gotArgs[i] != "--cap-drop" && gotArgs[i] != "--cap-add" && gotArgs[i] != "--security-opt" {
				continue
			}
			if i+1 >= len(gotArgs) {
				t.Fatalf("security flag %q has no value", gotArgs[i])
			}
			got = append(got, gotArgs[i], gotArgs[i+1])
			i++
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("security args with enableCgroups=%v = %#v, want %#v", enableCgroups, got, want)
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
			m := &Runtime{
				runnerConfig: &config.Config{},
				config: Config{
					DiskQuotaActive:    tc.active,
					DefaultDiskQuotaMB: 1024,
				},
			}
			if got := m.defaultLimits().DiskMB; got != tc.want {
				t.Errorf("defaultLimits().DiskMB = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestContainerStatePredicates(t *testing.T) {
	tests := []struct {
		name      string
		state     containerState
		wantReady bool
		wantStart bool
	}{
		{
			name:      "running",
			state:     containerState{Status: containerStatusRunning, Running: true},
			wantReady: true,
		},
		{
			name:      "created",
			state:     containerState{Status: containerStatusCreated},
			wantStart: true,
		},
		{
			name:      "exited",
			state:     containerState{Status: containerStatusExited},
			wantStart: true,
		},
		{
			name:  "paused",
			state: containerState{Status: containerStatusPaused, Running: true, Paused: true},
		},
		{
			name:  "restarting",
			state: containerState{Status: containerStatusRestarting, Restarting: true},
		},
		{
			name:  "dead",
			state: containerState{Status: containerStatusDead, Dead: true},
		},
		{
			name:  "removing",
			state: containerState{Status: containerStatusRemoving},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isContainerReady(tc.state); got != tc.wantReady {
				t.Fatalf("isContainerReady() = %v, want %v", got, tc.wantReady)
			}
			if got := canStartContainer(tc.state); got != tc.wantStart {
				t.Fatalf("canStartContainer() = %v, want %v", got, tc.wantStart)
			}
		})
	}
}

func TestEnsureSandboxRunningCleansUpStartedContainerOnWakeFailures(t *testing.T) {
	tests := []struct {
		name        string
		containerIP string
		ipErr       error
		policyErr   error
		waitErr     error
		wantEvents  []string
	}{
		{
			name:       "container ip fails",
			ipErr:      errors.New("no container ip"),
			wantEvents: []string{"find", "inspect", "start", "containerIP", "stop", "teardown"},
		},
		{
			name:        "apply policy fails",
			containerIP: "172.18.0.2",
			policyErr:   errors.New("iptables failed"),
			wantEvents:  []string{"find", "inspect", "start", "containerIP", "applyPolicy", "stop", "teardown"},
		},
		{
			name:        "wait for daemon fails",
			containerIP: "172.18.0.2",
			waitErr:     errors.New("daemon never ready"),
			wantEvents:  []string{"find", "inspect", "start", "containerIP", "applyPolicy", "waitForDaemon", "stop", "teardown"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			const containerID = "container-1"
			m := newRuntime(&config.Config{}, Config{}, &fakeDockerBackend{
				events:         &events,
				containerID:    containerID,
				ip:             tc.containerIP,
				containerIPErr: tc.ipErr,
			})
			m.applyPolicy = func(bridgeIface, gotID, sourceIP, gatewayIP string, port int) error {
				events = append(events, "applyPolicy")
				if gotID != containerID {
					t.Fatalf("applyPolicy containerID = %q, want %q", gotID, containerID)
				}
				return tc.policyErr
			}
			m.teardownRules = func(gotID string) error {
				events = append(events, "teardown")
				if gotID != containerID {
					t.Fatalf("teardownRules containerID = %q, want %q", gotID, containerID)
				}
				return nil
			}
			m.waitForDaemon = func(context.Context, string) error {
				events = append(events, "waitForDaemon")
				if tc.waitErr != nil {
					return tc.waitErr
				}
				return nil
			}

			_, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id")
			if err == nil {
				t.Fatal("expected wake to fail")
			}
			if !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("events = %v, want %v", events, tc.wantEvents)
			}
		})
	}
}

func TestEnsureSandboxRunningFailedWakeAfterNetworkDetachStopsContainerAndRemovesRules(t *testing.T) {
	events := []string{}
	const containerID = "container-1"
	m := newRuntime(&config.Config{}, Config{}, &fakeDockerBackend{
		events:      &events,
		containerID: containerID,
		containerIPErr: fmt.Errorf(
			"%w: container %s has no IP on %s",
			ErrSandboxNetworkUnavailable,
			containerID,
			runnerBridgeNetwork,
		),
	})
	m.applyPolicy = func(string, string, string, string, int) error {
		return fmt.Errorf("unexpected applyPolicy")
	}
	m.teardownRules = func(gotID string) error {
		events = append(events, "teardown")
		if gotID != containerID {
			t.Fatalf("teardownRules containerID = %q, want %q", gotID, containerID)
		}
		return nil
	}
	m.waitForDaemon = func(context.Context, string) error {
		return fmt.Errorf("unexpected waitForDaemon")
	}

	_, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id")
	if !errors.Is(err, ErrSandboxNetworkUnavailable) {
		t.Fatalf("ensureSandboxRunningOnce() error = %v, want %v", err, ErrSandboxNetworkUnavailable)
	}
	wantEvents := []string{"find", "inspect", "start", "containerIP", "stop", "teardown"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestEnsureSandboxRunningLeavesRulesWhenWakeCleanupCannotStopContainer(t *testing.T) {
	events := []string{}
	const containerID = "container-1"
	m := newRuntime(&config.Config{}, Config{}, &fakeDockerBackend{
		events:         &events,
		containerID:    containerID,
		containerIPErr: errors.New("no container ip"),
		stopErr:        errors.New("stop failed"),
	})
	m.teardownRules = func(string) error {
		events = append(events, "teardown")
		return nil
	}

	_, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id")
	if err == nil {
		t.Fatal("expected wake to fail")
	}
	wantEvents := []string{"find", "inspect", "start", "containerIP", "stop"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestEnsureSandboxRunningDoesNotCleanUpAfterSuccessfulWake(t *testing.T) {
	events := []string{}
	const containerID = "container-1"
	m := newRuntime(&config.Config{}, Config{}, &fakeDockerBackend{
		events:      &events,
		containerID: containerID,
		ip:          "172.18.0.2",
	})
	m.applyPolicy = func(string, string, string, string, int) error {
		events = append(events, "applyPolicy")
		return nil
	}
	m.teardownRules = func(string) error {
		return fmt.Errorf("unexpected teardown")
	}
	m.waitForDaemon = func(context.Context, string) error {
		events = append(events, "waitForDaemon")
		return nil
	}

	if _, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id"); err != nil {
		t.Fatalf("ensureSandboxRunningOnce() failed: %v", err)
	}
	wantEvents := []string{"find", "inspect", "start", "containerIP", "applyPolicy", "waitForDaemon"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
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
