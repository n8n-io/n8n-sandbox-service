package netrules

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

const chainPrefix = "N8N-SB-"

var mu sync.Mutex

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

func hostChainName(containerID string) string {
	return ChainName(containerID) + "-HOST"
}

// ApplyPolicy configures per-sandbox egress rules, ingress protection for the
// daemon port, and a block on reaching the runner host itself.
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
	for _, cidr := range netpolicy.PrivateRangesV4 {
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

	// The egress chain cannot protect the runner host. DOCKER-USER is only
	// consulted for forwarded packets, and anything addressed to a host-local
	// address (the bridge gateway, the host's own private IP) is delivered via
	// INPUT instead, so the private-range drops above never see it. Everything
	// reaching INPUT is locally destined by definition, so dropping this
	// container's new connections there covers every host address at once.
	// Established traffic is exempt: the runner dials the sandbox daemon, and
	// the replies arrive here.
	hostChain := hostChainName(containerID)
	if err := run("iptables", "-N", hostChain); err != nil {
		return fmt.Errorf("create host chain: %w", err)
	}
	if err := run("iptables", "-I", "INPUT", "1", "-s", sourceIP+"/32", "-j", hostChain); err != nil {
		return fmt.Errorf("insert host jump: %w", err)
	}
	if err := run("iptables", "-A", hostChain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("allow established host traffic: %w", err)
	}
	if err := run("iptables", "-A", hostChain, "-j", "DROP"); err != nil {
		return fmt.Errorf("append host drop: %w", err)
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

	if err := removeJumpReferences("DOCKER-USER", ChainName(containerID)); err != nil {
		return err
	}
	if err := removeJumpReferences("DOCKER-USER", ingressChainName(containerID)); err != nil {
		return err
	}
	if err := removeJumpReferences("INPUT", hostChainName(containerID)); err != nil {
		return err
	}

	for _, chain := range []string{ChainName(containerID), ingressChainName(containerID), hostChainName(containerID)} {
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

func removeJumpReferences(parent, chain string) error {
	out, err := output("iptables", "-S", parent)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no chain/target/match by that name") {
			return nil
		}
		return err
	}

	prefix := "-A " + parent + " "
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, "-j "+chain) {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(trimmed, prefix))
		args = append([]string{"-D", parent}, args...)
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
