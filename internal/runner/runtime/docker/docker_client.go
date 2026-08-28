package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	runnerBridgeNetwork      = "runner-bridge"
	bridgeNameOption         = "com.docker.network.bridge.name"
	containerLabelManaged    = "sandbox-service.managed"
	containerLabelManagedVal = "true"
	containerLabelSandboxID  = "sandbox-service.id"

	// managedLabelFilter selects the containers this runner owns, and is what keeps
	// every lookup and the death watcher agreeing on that set.
	managedLabelFilter = "label=" + containerLabelManaged + "=" + containerLabelManagedVal
)

type containerInspect struct {
	ID     string         `json:"Id"`
	Name   string         `json:"Name"`
	State  containerState `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type containerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	Dead       bool   `json:"Dead"`
}

// containerEvent is the subset of a `docker events` record this runtime reads.
// Actor.ID is the container, and Actor.Attributes carries the container's labels,
// which is how a die event names the sandbox that died without a second lookup —
// the container is often already gone by the time the event is read.
type containerEvent struct {
	Actor struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

type networkInspect struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
	IPAM    struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

type dockerBackend interface {
	ping(ctx context.Context) error
	createContainer(ctx context.Context, sandboxID, containerName, image string, limits *ResourceLimits, enableCgroups bool) (string, error)
	startContainer(ctx context.Context, containerID string) error
	stopContainer(ctx context.Context, containerID string) error
	removeContainer(ctx context.Context, containerID string) error
	containerIP(ctx context.Context, containerID string) (string, error)
	inspectContainer(ctx context.Context, containerID string) (*containerInspect, error)
	inspectNetwork(ctx context.Context, name string) (*networkInspect, error)
	findContainerByLabels(ctx context.Context, filterArgs ...string) ([]string, error)
	pullImage(ctx context.Context, image string) error
	watchContainerDeaths(ctx context.Context, onDie func(containerID, sandboxID string)) error
	run(ctx context.Context, args ...string) (string, error)
}

// dockerClient is a thin wrapper around the docker CLI. It is the only place
// in the Docker runtime package that shells out to docker.
type dockerClient struct {
	host string
}

func (dc *dockerClient) run(ctx context.Context, args ...string) (string, error) {
	return dc.runWithStdin(ctx, nil, args...)
}

func (dc *dockerClient) runWithStdin(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+dc.host)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("docker %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return stdout.String(), nil
}

func (dc *dockerClient) ping(ctx context.Context) error {
	_, err := dc.run(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}

func (dc *dockerClient) createContainer(ctx context.Context, sandboxID, containerName, image string, limits *ResourceLimits, enableCgroups bool) (string, error) {
	args := dockerContainerCreateArgs(sandboxID, containerName, image, limits, enableCgroups)

	out, err := dc.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func dockerContainerCreateArgs(sandboxID, containerName, image string, limits *ResourceLimits, enableCgroups bool) []string {
	args := []string{
		"container", "create",
		"--name", containerName,
		"--hostname", "sandbox",
		"--restart", "unless-stopped",
		"--network", runnerBridgeNetwork,
		"--label", containerLabelManaged + "=" + containerLabelManagedVal,
		"--label", containerLabelSandboxID + "=" + sandboxID,
		"--user", "1000:1000",
		"--env", "HOME=/home/user",
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	args = append(args, dockerSandboxCapabilityArgs()...)
	args = append(args,
		// netrules only filter IPv4; disable v6 in the container so sandboxes
		// can't bypass the policy via link-local/ULA or v6 metadata addresses.
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.lo.disable_ipv6=1",
	)
	if enableCgroups {
		args = append(args, dockerLimitArgs(limits)...)
	}
	args = append(args, dockerDiskQuotaArgs(limits)...)
	args = append(args, image)
	return args
}

// dockerSandboxCapabilityArgs is the single capability policy for every
// Docker-backed sandbox. Keep the allowlist minimal while preserving
// passwordless sudo for common package installation workflows.
//
// Never create a sandbox container with --privileged. Docker ignores --cap-drop
// for a privileged container, so that flag would void this policy silently.
func dockerSandboxCapabilityArgs() []string {
	return []string{
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "DAC_OVERRIDE",
		"--cap-add", "FOWNER",
		"--cap-add", "SETGID",
		"--cap-add", "SETUID",
	}
}

func (dc *dockerClient) startContainer(ctx context.Context, containerID string) error {
	_, err := dc.run(ctx, "container", "start", containerID)
	return err
}

func (dc *dockerClient) stopContainer(ctx context.Context, containerID string) error {
	_, err := dc.run(ctx, "container", "stop", "-t", "10", containerID)
	return err
}

func (dc *dockerClient) removeContainer(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	_, err := dc.run(ctx, "container", "rm", "-f", containerID)
	if isDockerNotFound(err) {
		return nil
	}
	return err
}

func (dc *dockerClient) containerIP(ctx context.Context, containerID string) (string, error) {
	inspect, err := dc.inspectContainer(ctx, containerID)
	if err != nil {
		return "", err
	}
	network, ok := inspect.NetworkSettings.Networks[runnerBridgeNetwork]
	if !ok || network.IPAddress == "" {
		return "", fmt.Errorf("container %s has no IP on %s", containerID, runnerBridgeNetwork)
	}
	return network.IPAddress, nil
}

func (dc *dockerClient) inspectContainer(ctx context.Context, containerID string) (*containerInspect, error) {
	out, err := dc.run(ctx, "container", "inspect", containerID)
	if err != nil {
		return nil, err
	}
	var items []containerInspect
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("decode container inspect: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("container inspect returned no results for %s", containerID)
	}
	return &items[0], nil
}

func (dc *dockerClient) inspectNetwork(ctx context.Context, name string) (*networkInspect, error) {
	out, err := dc.run(ctx, "network", "inspect", name)
	if err != nil {
		return nil, err
	}
	var items []networkInspect
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("decode network inspect: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("network inspect returned no results for %s", name)
	}
	return &items[0], nil
}

func (dc *dockerClient) findContainerByLabels(ctx context.Context, filterArgs ...string) ([]string, error) {
	out, err := dc.run(ctx, containerIDArgs(filterArgs...)...)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// containerIDArgs builds the lookup every container ID in this package comes from,
// so there is one place that decides how wide those IDs are.
//
// --no-trunc is the whole reason it exists. docker ps abbreviates IDs to 12
// characters, while docker create and the die events from docker events both report
// the full 64, and the runner matches one against the other: a stop it recorded
// under an abbreviated ID never matches the death that stop caused, so the runner
// reads its own deliberate stop as a crash and the next request is refused with
// 409 sandbox_restarted. Nothing downstream notices the difference otherwise —
// docker resolves ID prefixes, and netrules truncates to 12 for its chain names.
func containerIDArgs(filters ...string) []string {
	args := make([]string, 0, 3+2*len(filters))
	args = append(args, "ps", "-aq", "--no-trunc")
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	return args
}

func (dc *dockerClient) pullImage(ctx context.Context, image string) error {
	if _, err := dc.run(ctx, "image", "inspect", image); err == nil {
		slog.Info("image already present, skipping pull", "image", image)
		return nil
	}
	_, err := dc.run(ctx, "pull", image)
	return err
}

// watchContainerDeaths calls onDie for every managed sandbox container that exits,
// until ctx is canceled or the stream breaks. It is the only long-running docker
// invocation in this package, so it streams rather than buffering like run does.
//
// The filters are the daemon's, not ours: asking it for die events on managed
// containers means the runner is not woken for every image pull and exec on the
// host. Callers get the events from the moment this connects — a death during a
// reconnect is not replayed, which is why the wake path still repairs a container
// it finds restarted rather than trusting the event alone.
func (dc *dockerClient) watchContainerDeaths(ctx context.Context, onDie func(containerID, sandboxID string)) error {
	cmd := exec.CommandContext(ctx, "docker", "events",
		"--filter", "type=container",
		"--filter", "event=die",
		"--filter", managedLabelFilter,
		"--format", "{{json .}}")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+dc.host)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe docker events: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start docker events: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var event containerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("decode docker event", "err", err)
			continue
		}
		onDie(event.Actor.ID, event.Actor.Attributes[containerLabelSandboxID])
	}
	// Wait after draining, or the pipe closes under the scanner. A canceled ctx kills
	// the process, so the error it reports then is the cancellation, not a failure.
	waitErr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("read docker events: %w", scanErr)
	}
	return fmt.Errorf("docker events exited: %s: %w", strings.TrimSpace(stderr.String()), waitErr)
}

func firstGateway(inspect *networkInspect) string {
	if inspect != nil && len(inspect.IPAM.Config) > 0 {
		return inspect.IPAM.Config[0].Gateway
	}
	return ""
}

// bridgeInterface returns the host interface backing a Docker bridge network.
// Networks this runner creates carry the name as an option; ones created by an
// earlier version do not, and Docker then derives the device from the network
// ID. netrules verifies the result exists before it builds rules on it.
func bridgeInterface(inspect *networkInspect) string {
	if inspect == nil {
		return ""
	}
	if name := inspect.Options[bridgeNameOption]; name != "" {
		return name
	}
	if len(inspect.ID) >= 12 {
		return "br-" + inspect.ID[:12]
	}
	return ""
}

func dockerDiskQuotaArgs(limits *ResourceLimits) []string {
	if limits == nil || limits.DiskMB <= 0 {
		return nil
	}
	return []string{"--storage-opt", fmt.Sprintf("size=%dm", limits.DiskMB)}
}

func dockerLimitArgs(limits *ResourceLimits) []string {
	if limits == nil {
		return nil
	}

	args := make([]string, 0, 6)
	if limits.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", limits.MemoryMB))
	}
	if limits.CPUPercent > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(limits.CPUPercent)/100, 'f', 2, 64))
	}
	if limits.PidsMax > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(limits.PidsMax))
	}
	return args
}

func isDockerNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such network") ||
		strings.Contains(msg, "no such image") ||
		strings.Contains(msg, "not found")
}
