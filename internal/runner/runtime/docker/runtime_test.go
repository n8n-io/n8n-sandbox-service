package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

type fakeDockerBackend struct {
	events         *[]string
	containerID    string
	ip             string
	containerIPErr error
	stopErr        error
	// network the container was created on, and the one inspect reports.
	network string
	// image and limits of the last create.
	image  string
	limits *ResourceLimits
	// changes of the last import.
	changes []string
	// absent: no container for the sandbox until one is created.
	absent bool
}

func (f *fakeDockerBackend) ping(context.Context) error {
	return errors.New("unexpected ping")
}

func (f *fakeDockerBackend) createContainer(_ context.Context, _, _, _, image, network string, limits *ResourceLimits, _ bool) (string, error) {
	*f.events = append(*f.events, "create")
	f.absent = false
	f.network = network
	f.image, f.limits = image, limits
	return f.containerID, nil
}

func (f *fakeDockerBackend) startContainer(context.Context, string) error {
	*f.events = append(*f.events, "start")
	return nil
}

func (f *fakeDockerBackend) stopContainer(context.Context, string) error {
	*f.events = append(*f.events, "stop")
	return f.stopErr
}

func (f *fakeDockerBackend) removeContainer(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	*f.events = append(*f.events, "remove")
	return nil
}

func (f *fakeDockerBackend) containerIP(context.Context, string) (string, error) {
	*f.events = append(*f.events, "containerIP")
	if f.containerIPErr != nil {
		return "", f.containerIPErr
	}
	return f.ip, nil
}

func (f *fakeDockerBackend) exportContainer(_ context.Context, _ string, w io.Writer) error {
	*f.events = append(*f.events, "export")
	_, err := io.WriteString(w, "TAR")
	return err
}

func (f *fakeDockerBackend) importImage(_ context.Context, r io.Reader, image string, changes []string) error {
	b, _ := io.ReadAll(r)
	*f.events = append(*f.events, "import:"+image+":"+string(b))
	f.changes = changes
	return nil
}

func (f *fakeDockerBackend) imageConfig(context.Context, string) (*imageRunConfig, error) {
	return &imageRunConfig{Cmd: []string{"/sandbox-daemon"}, Env: []string{"PATH=/usr/bin:/bin", "A=b c"}, User: "1000:1000", WorkingDir: "/home/user"}, nil
}

func (f *fakeDockerBackend) removeImage(_ context.Context, image string) error {
	*f.events = append(*f.events, "rmi:"+image)
	return nil
}

func (f *fakeDockerBackend) inspectContainer(context.Context, string) (*containerInspect, error) {
	*f.events = append(*f.events, "inspect")
	inspect := &containerInspect{
		ID:    f.containerID,
		State: containerState{Status: containerStatusExited},
	}
	if f.network != "" {
		// A stopped container keeps its network attachment; only the address is gone.
		inspect.NetworkSettings.Networks = map[string]struct {
			IPAddress string `json:"IPAddress"`
		}{f.network: {}}
	}
	return inspect, nil
}

func (f *fakeDockerBackend) inspectNetwork(context.Context, string) (*networkInspect, error) {
	return nil, errors.New("unexpected inspectNetwork")
}

func (f *fakeDockerBackend) findContainerByLabels(context.Context, ...string) ([]string, error) {
	*f.events = append(*f.events, "find")
	if f.absent {
		return nil, nil
	}
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

func TestDockerContainerCreateArgs(t *testing.T) {
	limits := &ResourceLimits{
		MemoryMB:   512,
		CPUPercent: 150,
		PidsMax:    128,
		DiskMB:     1024,
	}

	got := dockerContainerCreateArgs("sandbox-id", "default", "sandbox-name", "sandbox-image", runnerNoEgressNetwork, limits, true, []string{"1.1.1.1", "8.8.8.8"}, false)
	want := []string{
		"container", "create",
		"--name", "sandbox-name",
		"--hostname", "sandbox",
		"--restart", "unless-stopped",
		"--network", runnerNoEgressNetwork,
		"--label", containerLabelManaged + "=" + containerLabelManagedVal,
		"--label", containerLabelSandboxID + "=sandbox-id",
		"--label", containerLabelTemplate + "=default",
		"--user", "1000:1000",
		"--env", "HOME=/home/user",
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.lo.disable_ipv6=1",
		"--dns", "1.1.1.1",
		"--dns", "8.8.8.8",
		"--dns-search", ".",
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

// IPv6 is assumed present and disabled in the container, unless the host is
// IPv4-only: there the net.ipv6 sysctls don't exist and docker refuses to
// create the container.
func TestDockerContainerCreateArgsSkipIPv6SysctlsOnIPv4OnlyHost(t *testing.T) {
	sysctls := func(args []string) []string {
		var got []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--sysctl" {
				got = append(got, args[i+1])
				i++
			}
		}
		return got
	}

	got := sysctls(dockerContainerCreateArgs("sandbox-id", "default", "sandbox-name", "sandbox-image", runnerBridgeNetwork, nil, false, nil, false))
	want := []string{
		"net.ipv6.conf.all.disable_ipv6=1",
		"net.ipv6.conf.default.disable_ipv6=1",
		"net.ipv6.conf.lo.disable_ipv6=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sysctls by default = %#v, want %#v", got, want)
	}

	if got := sysctls(dockerContainerCreateArgs("sandbox-id", "default", "sandbox-name", "sandbox-image", runnerBridgeNetwork, nil, false, nil, true)); got != nil {
		t.Fatalf("sysctls on IPv4-only host = %#v, want nil", got)
	}
}

// Only an explicit ipv6.disable=1 skips the sysctls. A missing parameter file
// (IPv6 module not loaded) keeps IPv6 disabled in the container.
func TestHostOnlyIPv4(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"1\n", true},
		{"0\n", false},
		{"", false},
	} {
		path := filepath.Join(dir, "disable")
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := hostOnlyIPv4(path); got != tc.want {
			t.Fatalf("hostOnlyIPv4 with %q = %v, want %v", tc.content, got, tc.want)
		}
	}
	if hostOnlyIPv4(filepath.Join(dir, "missing")) {
		t.Fatal("hostOnlyIPv4 for a missing file = true, want false")
	}
}

func TestExplainIPv6SysctlError(t *testing.T) {
	if explainIPv6SysctlError(nil) != nil {
		t.Fatal("explainIPv6SysctlError(nil) != nil")
	}

	other := errors.New("docker container start abc: Error response from daemon: no such container")
	if got := explainIPv6SysctlError(other); got != other {
		t.Fatalf("unrelated error rewritten: %v", got)
	}

	sysctlErr := errors.New("docker container start abc: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: open sysctl net.ipv6.conf.default.disable_ipv6 file: unsafe procfs detected: openat2 fsmount:fscontext:proc/./sys/net/ipv6/conf/default/disable_ipv6: no such file or directory: exit status 1")
	got := explainIPv6SysctlError(sysctlErr)
	if !errors.Is(got, sysctlErr) {
		t.Fatalf("explained error does not wrap the original: %v", got)
	}
	if !strings.Contains(got.Error(), "ipv6 module is loaded") {
		t.Fatalf("explained error lacks the cause: %v", got)
	}
}

// Without configured servers the container keeps Docker's default DNS, which
// is what runners did before the setting existed.
func TestDockerDNSArgsOnlyWhenConfigured(t *testing.T) {
	if got := dockerDNSArgs(nil); got != nil {
		t.Fatalf("dockerDNSArgs(nil) = %#v, want nil", got)
	}
	got := dockerDNSArgs([]string{"8.8.8.8"})
	want := []string{"--dns", "8.8.8.8", "--dns-search", "."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dockerDNSArgs() = %#v, want %#v", got, want)
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
		gotArgs := dockerContainerCreateArgs("sandbox-id", "default", "sandbox-name", "sandbox-image", runnerBridgeNetwork, nil, enableCgroups, nil, false)
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
			m.applyPolicy = func(bridgeIface, gotID, sourceIP, gatewayIP string, port int, _ bool) error {
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

func TestCreateContainerCleansUpAfterContextCancellation(t *testing.T) {
	events := []string{}
	const containerID = "container-1"
	m := newRuntime(&config.Config{}, Config{}, &fakeDockerBackend{
		events:      &events,
		containerID: containerID,
		ip:          "172.18.0.2",
		absent:      true,
	})
	m.imageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error {
		events = append(events, "applyPolicy")
		return nil
	}
	m.teardownRules = func(string) error {
		events = append(events, "teardown")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.waitForDaemon = func(ctx context.Context, _ string) error {
		events = append(events, "waitForDaemon")
		cancel()
		return ctx.Err()
	}

	if _, err := m.CreateContainer(ctx, "sandbox-id-1234", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateContainer() error = %v, want %v", err, context.Canceled)
	}
	wantEvents := []string{"find", "create", "start", "containerIP", "applyPolicy", "waitForDaemon", "remove", "teardown"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

// The egress mode picks the network a container is created on, and the policy
// applied on create and again on wake is that network's: the bridge the wake
// reads from the stopped container's attachment, not a runner-wide default.
func TestCreateAndWakeApplyTheNetworkPolicyOfTheSandboxEgress(t *testing.T) {
	type policyCall struct {
		bridgeIface, gatewayIP string
		blockEgress            bool
	}
	for _, tc := range []struct {
		name        string
		opts        *runnerruntime.CreateOptions
		wantNetwork string
		wantPolicy  policyCall
	}{
		{
			name:        "default is the public bridge",
			opts:        nil,
			wantNetwork: runnerBridgeNetwork,
			wantPolicy:  policyCall{"runner-bridge", "172.18.0.1", false},
		},
		{
			name:        "egress none is the no-egress bridge",
			opts:        &runnerruntime.CreateOptions{Egress: runnerruntime.EgressNone},
			wantNetwork: runnerNoEgressNetwork,
			wantPolicy:  policyCall{runnerNoEgressBridge, "172.19.0.1", true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.18.0.2", absent: true}
			m := newRuntime(&config.Config{}, Config{}, backend)
			m.publicNetwork.iface, m.publicNetwork.gatewayIP = "runner-bridge", "172.18.0.1"
			m.noEgressNetwork.iface, m.noEgressNetwork.gatewayIP = runnerNoEgressBridge, "172.19.0.1"
			m.imageReady.Store(true)
			var policies []policyCall
			m.applyPolicy = func(bridgeIface, _, _, gatewayIP string, _ int, blockEgress bool) error {
				policies = append(policies, policyCall{bridgeIface, gatewayIP, blockEgress})
				return nil
			}
			m.teardownRules = func(string) error { return nil }
			m.waitForDaemon = func(context.Context, string) error { return nil }

			if _, err := m.CreateContainer(context.Background(), "sandbox-id-1234", tc.opts); err != nil {
				t.Fatalf("CreateContainer() failed: %v", err)
			}
			if backend.network != tc.wantNetwork {
				t.Fatalf("created on network %q, want %q", backend.network, tc.wantNetwork)
			}
			if _, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id-1234"); err != nil {
				t.Fatalf("ensureSandboxRunningOnce() failed: %v", err)
			}
			want := []policyCall{tc.wantPolicy, tc.wantPolicy}
			if !reflect.DeepEqual(policies, want) {
				t.Fatalf("policies applied = %+v, want %+v", policies, want)
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
			"%w: container %s has no IP on a runner network",
			ErrSandboxNetworkUnavailable,
			containerID,
		),
	})
	m.applyPolicy = func(string, string, string, string, int, bool) error {
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
	m.applyPolicy = func(string, string, string, string, int, bool) error {
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

// The target would pass the readiness check, so only an unfollowed redirect keeps
// waitForDaemon waiting. The 307 would carry the exec body along if followed.
func TestWaitForDaemonRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"seq":0,"type":"exit","exit_code":0}` + "\n"))
	}))
	defer target.Close()
	var daemonHits atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemonHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer daemon.Close()

	// Long enough for a few of waitForDaemon's 200ms polls.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForDaemon(ctx, daemon.URL); err == nil {
		t.Fatal("waitForDaemon() succeeded through a redirect")
	}
	if got := daemonHits.Load(); got == 0 {
		t.Fatal("daemon served no request, so no redirect was refused")
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

// A create boots its template's image with its limits, an unset limit taking
// the default; Templates lists the default first.
// A second create for an id that has a container is refused, whatever name
// the first one carries.
func TestCreateRefusesAnIDThatHasAContainer(t *testing.T) {
	events := []string{}
	m := newRuntime(&config.Config{}, Config{SandboxImage: "img"}, &fakeDockerBackend{events: &events, containerID: "container-1"})
	m.imageReady.Store(true)
	if _, err := m.CreateContainer(context.Background(), "11111111-1111-4111-8111-111111111111", nil); err == nil {
		t.Fatal("create succeeded")
	}
	if !reflect.DeepEqual(events, []string{"find"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestCreateUsesTemplateImageAndLimits(t *testing.T) {
	events := []string{}
	backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.18.0.2", absent: true}
	m := newRuntime(&config.Config{}, Config{
		SandboxImage: "img/default", DefaultMemoryMB: 512, DefaultCPUPercent: 100, DefaultPidsMax: 256,
		DiskQuotaActive: true, DefaultDiskQuotaMB: 2048,
		Templates: map[string]Template{"python": {Image: "img/python", MemoryMB: 4096, CPUPercent: 250, DiskQuotaMB: 20480}},
	}, backend)
	m.imageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error { return nil }

	if _, err := m.CreateContainer(context.Background(), "11111111-1111-4111-8111-111111111111", &CreateOptions{Template: "python"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.image != "img/python" || backend.limits.MemoryMB != 4096 || backend.limits.CPUPercent != 250 ||
		backend.limits.PidsMax != 256 || backend.limits.DiskMB != 20480 {
		t.Fatalf("image %q limits %+v", backend.image, backend.limits)
	}
	backend.absent = true
	if _, err := m.CreateContainer(context.Background(), "22222222-2222-4222-8222-222222222222", &CreateOptions{}); err != nil {
		t.Fatalf("default create: %v", err)
	}
	if backend.image != "img/default" || backend.limits.MemoryMB != 512 || backend.limits.DiskMB != 2048 {
		t.Fatalf("default: image %q limits %+v", backend.image, backend.limits)
	}
	if _, err := m.CreateContainer(context.Background(), "33333333-3333-4333-8333-333333333333", &CreateOptions{Template: "go"}); err == nil {
		t.Fatal("unknown template created")
	}
	got := m.Templates()
	if len(got) != 2 || got[0].Name != "default" || got[1] != (runnerruntime.TemplateInfo{Name: "python", VCPUs: 3, MemoryMB: 4096, DiskMB: 20480}) {
		t.Fatalf("templates = %+v", got)
	}
}

// An egress-proxy sandbox goes on the proxy network, and its address names it
// to the proxy until it stops or is deleted.
func TestProxySandboxIsIdentifiedByItsAddress(t *testing.T) {
	events := []string{}
	backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.20.0.5", absent: true}
	m := newRuntime(&config.Config{}, Config{SandboxImage: "img", EgressProxyPort: 3128}, backend)
	m.proxyNetwork.iface, m.proxyNetwork.gatewayIP = runnerEgressProxyBridge, "172.20.0.1"
	m.imageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error { return nil }

	const id = "11111111-1111-4111-8111-111111111111"
	if _, err := m.CreateContainer(context.Background(), id, &CreateOptions{Egress: runnerruntime.EgressProxy}); err != nil {
		t.Fatal(err)
	}
	if backend.network != runnerEgressProxyNetwork {
		t.Fatalf("network = %q", backend.network)
	}
	if got, ok := m.identifyProxyClient("172.20.0.5:40000"); !ok || got != id {
		t.Fatalf("identify = %q %v", got, ok)
	}
	if u, ok := m.EgressProxyURL(id); !ok || u != "http://172.20.0.1:3128" {
		t.Fatalf("proxy url = %q %v", u, ok)
	}
	if _, ok := m.identifyProxyClient("172.20.0.6:40000"); ok {
		t.Fatal("unknown address identified")
	}
	// A later sandbox taking the address takes its identity.
	m.trackProxyAddr(&m.proxyNetwork, "22222222-2222-4222-8222-222222222222", "172.20.0.5")
	if got, _ := m.identifyProxyClient("172.20.0.5:1"); got != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("after reuse = %q", got)
	}
	if _, ok := m.EgressProxyURL(id); ok {
		t.Fatal("first sandbox kept a proxy url after losing its address")
	}
	m.untrackProxy("22222222-2222-4222-8222-222222222222")
	if _, ok := m.identifyProxyClient("172.20.0.5:1"); ok {
		t.Fatal("untracked address still identified")
	}
	// A public sandbox has no proxy identity.
	m.trackProxyAddr(&m.publicNetwork, id, "172.18.0.9")
	if _, ok := m.identifyProxyClient("172.18.0.9:1"); ok {
		t.Fatal("public sandbox identified")
	}
}

// An export streams the stopped container; an import makes the sandbox's own
// image with the template's run config and creates from it.
func TestExportAndImport(t *testing.T) {
	events := []string{}
	backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.18.0.2"}
	m := newRuntime(&config.Config{}, Config{SandboxImage: "img/default"}, backend)
	m.imageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error { return nil }
	const id = "11111111-1111-4111-8111-111111111111"

	var buf strings.Builder
	if err := m.ExportSandbox(context.Background(), id, &buf); err != nil || buf.String() != "TAR" {
		t.Fatalf("export: %q %v", buf.String(), err)
	}
	backend.absent = true // the archive removed it
	if _, err := m.ImportSandbox(context.Background(), id, &CreateOptions{}, strings.NewReader("TAR")); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !slices.Contains(events, "import:sandbox-restored:"+id+":TAR") || backend.image != "sandbox-restored:"+id {
		t.Fatalf("events %v, image %q", events, backend.image)
	}
	want := []string{`CMD ["/sandbox-daemon"]`, `ENV PATH="/usr/bin:/bin"`, `ENV A="b c"`, "USER 1000:1000", "WORKDIR /home/user"}
	if !slices.Equal(backend.changes, want) {
		t.Fatalf("changes = %q", backend.changes)
	}
	if err := m.DeleteSandbox(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(events, "rmi:sandbox-restored:"+id) {
		t.Fatalf("restored image not removed: %v", events)
	}
}

// Under a disk quota an archive is at most the template's image and quota:
// sparse files come out of an export at full size, so the cap is on the tar.
func TestArchiveLimit(t *testing.T) {
	backend := &fakeDockerBackend{events: &[]string{}, containerID: "container-1"}
	m := newRuntime(&config.Config{}, Config{SandboxImage: "img/default", DiskQuotaActive: true, DefaultDiskQuotaMB: 1}, backend)
	limit, err := m.archiveLimit(context.Background(), "")
	if want := int64(1<<20 + (1<<20)/10 + 64<<20); err != nil || limit != want {
		t.Fatalf("limit = %d %v, want %d", limit, err, want)
	}
	if _, err := io.Copy(&capWriter{w: io.Discard, left: 10}, strings.NewReader(strings.Repeat("x", 11))); !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("writer over the cap: %v", err)
	}
	if _, err := io.Copy(io.Discard, &capReader{r: strings.NewReader(strings.Repeat("x", 11)), left: 10}); !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("reader over the cap: %v", err)
	}
	if n, err := io.Copy(io.Discard, &capReader{r: strings.NewReader("0123456789"), left: 10}); err != nil || n != 10 {
		t.Fatalf("reader at the cap: %d %v", n, err)
	}
	m = newRuntime(&config.Config{}, Config{SandboxImage: "img/default"}, backend)
	if limit, _ := m.archiveLimit(context.Background(), ""); limit != 0 {
		t.Fatalf("no quota, limit %d", limit)
	}
}
