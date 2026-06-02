package netrules

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const chainPrefix = "N8N-SB-"

var mu sync.Mutex

var privateRangesV4 = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
	"198.18.0.0/15",
	"240.0.0.0/4",
}

// ChainName returns the per-sandbox egress chain name.
func ChainName(containerID string) string {
	short := containerID
	if len(short) > 12 {
		short = short[:12]
	}
	return chainPrefix + short
}

func ingressChainName(containerID string) string {
	return ChainName(containerID) + "-IN"
}

// ApplyPolicy configures per-sandbox egress rules plus ingress protection for
// the daemon port.
func ApplyPolicy(containerID, sourceIP, gatewayIP string, daemonPort int) error {
	if containerID == "" {
		return fmt.Errorf("container id is required")
	}
	if sourceIP == "" {
		return fmt.Errorf("source ip is required")
	}

	// Serialize all iptables mutations so concurrent sandbox lifecycles
	// cannot observe the shared DOCKER-USER chain in an intermediate state.
	mu.Lock()
	defer mu.Unlock()

	if err := ensureDockerUserChain(); err != nil {
		return err
	}
	if err := teardownLocked(containerID); err != nil {
		return err
	}

	egressChain := ChainName(containerID)
	ingressChain := ingressChainName(containerID)

	if err := run("iptables", "-N", egressChain); err != nil {
		return fmt.Errorf("create egress chain: %w", err)
	}
	if err := run("iptables", "-I", "DOCKER-USER", "1", "-s", sourceIP+"/32", "-j", egressChain); err != nil {
		return fmt.Errorf("insert egress jump: %w", err)
	}
	if err := run("iptables", "-A", egressChain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("allow established traffic: %w", err)
	}
	for _, cidr := range privateRangesV4 {
		if err := run("iptables", "-A", egressChain, "-d", cidr, "-j", "DROP"); err != nil {
			return fmt.Errorf("drop private range %s: %w", cidr, err)
		}
	}
	if err := run("iptables", "-A", egressChain, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("append default accept: %w", err)
	}

	if err := run("iptables", "-N", ingressChain); err != nil {
		return fmt.Errorf("create ingress chain: %w", err)
	}
	if err := run("iptables", "-I", "DOCKER-USER", "1", "-d", sourceIP+"/32", "-p", "tcp", "--dport", fmt.Sprintf("%d", daemonPort), "-j", ingressChain); err != nil {
		return fmt.Errorf("insert ingress jump: %w", err)
	}
	if gatewayIP != "" {
		if err := run("iptables", "-A", ingressChain, "-s", gatewayIP+"/32", "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("allow daemon ingress from gateway: %w", err)
		}
	}
	if err := run("iptables", "-A", ingressChain, "-j", "DROP"); err != nil {
		return fmt.Errorf("append ingress drop: %w", err)
	}

	return nil
}

// Teardown removes per-sandbox iptables rules and chains.
func Teardown(containerID string) error {
	mu.Lock()
	defer mu.Unlock()
	return teardownLocked(containerID)
}

func teardownLocked(containerID string) error {
	if containerID == "" {
		return nil
	}

	if err := removeJumpReferences(ChainName(containerID)); err != nil {
		return err
	}
	if err := removeJumpReferences(ingressChainName(containerID)); err != nil {
		return err
	}

	for _, chain := range []string{ChainName(containerID), ingressChainName(containerID)} {
		_ = run("iptables", "-F", chain)
		_ = run("iptables", "-X", chain)
	}

	return nil
}

func ensureDockerUserChain() error {
	if err := run("iptables", "-n", "-L", "DOCKER-USER"); err != nil {
		return fmt.Errorf("inspect DOCKER-USER chain: %w", err)
	}
	return nil
}

func removeJumpReferences(chain string) error {
	out, err := output("iptables", "-S", "DOCKER-USER")
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no chain/target/match by that name") {
			return nil
		}
		return err
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, "-j "+chain) {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-A DOCKER-USER ") {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(trimmed, "-A DOCKER-USER "))
		args = append([]string{"-D", "DOCKER-USER"}, args...)
		_ = run("iptables", args...)
	}
	return nil
}

func run(name string, args ...string) error {
	_, err := output(name, args...)
	return err
}

func output(name string, args ...string) (string, error) {
	// -w 5: wait up to 5s for the kernel xtables lock instead of failing immediately.
	// -W 10000: poll the lock every 10ms (legacy iptables only; ignored by iptables-nft).
	if name == "iptables" {
		args = append([]string{"-w", "5", "-W", "10000"}, args...)
	}
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, err)
	}
	return stdout.String(), nil
}
