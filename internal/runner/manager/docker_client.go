package manager

import (
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
	containerLabelManaged    = "sandbox-service.managed"
	containerLabelManagedVal = "true"
	containerLabelSandboxID  = "sandbox-service.id"
)

type containerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type networkInspect struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
	IPAM    struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// dockerClient is a thin wrapper around the docker CLI. It is the only place
// in the manager package that shells out to docker.
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
		// netrules only filter IPv4; disable v6 in the container so sandboxes
		// can't bypass the policy via link-local/ULA or v6 metadata addresses.
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"--sysctl", "net.ipv6.conf.lo.disable_ipv6=1",
	}
	if enableCgroups {
		args = append(args, dockerLimitArgs(limits)...)
	}
	args = append(args, dockerDiskQuotaArgs(limits)...)
	args = append(args, image)

	out, err := dc.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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

func (dc *dockerClient) listContainersByLabel(ctx context.Context, label, value string) ([]string, error) {
	out, err := dc.run(ctx, "ps", "-aq", "--filter", "label="+label+"="+value)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

func (dc *dockerClient) findContainerByLabels(ctx context.Context, filterArgs ...string) ([]string, error) {
	args := []string{"ps", "-aq"}
	for _, f := range filterArgs {
		args = append(args, "--filter", f)
	}
	out, err := dc.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

func (dc *dockerClient) pullImage(ctx context.Context, image string) error {
	if _, err := dc.run(ctx, "image", "inspect", image); err == nil {
		slog.Info("image already present, skipping pull", "image", image)
		return nil
	}
	_, err := dc.run(ctx, "pull", image)
	return err
}

func firstGateway(inspect *networkInspect) string {
	if inspect != nil && len(inspect.IPAM.Config) > 0 {
		return inspect.IPAM.Config[0].Gateway
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
