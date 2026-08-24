package netrules

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

const chainPrefix = "N8N-SB-"

// Chains shared by every container on the runner bridge.
const (
	bridgeEgressChain = chainPrefix + "BR-EGRESS"
	bridgeHostChain   = chainPrefix + "BR-HOST"
)

var (
	mu sync.Mutex
	// Whether this process has already rebuilt the shared chains from empty.
	// Guarded by mu.
	sharedChainsReset bool
)

// ChainName returns the base name for a sandbox's own chains.
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

// EnsureBridgePolicy builds the chains shared by every container on the runner
// bridge. Call it at runner startup, before the first sandbox exists: the
// initial build flushes those chains (see resetSharedChains), and a flush while
// containers are on the bridge would leave them unfiltered until the
// replacement rules are back.
func EnsureBridgePolicy(bridgeIface string) error {
	mu.Lock()
	defer mu.Unlock()

	if err := ensureDockerUserChain(); err != nil {
		return err
	}
	return ensureBridgePolicy(bridgeIface)
}

// ApplyPolicy configures the policy shared by every container on the runner
// bridge plus ingress protection for this container's daemon port.
func ApplyPolicy(bridgeIface, containerID, sourceIP, gatewayIP string, daemonPort int) error {
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
	if err := ensureBridgePolicy(bridgeIface); err != nil {
		return fmt.Errorf("ensure bridge policy: %w", err)
	}
	if err := teardownLocked(containerID); err != nil {
		return err
	}

	ingressChain := ingressChainName(containerID)

	if err := run("iptables", "-N", ingressChain); err != nil {
		return fmt.Errorf("create ingress chain: %w", err)
	}
	if err := run("iptables", "-I", "DOCKER-USER", "1", "-d", sourceIP+"/32", "-p", "tcp", "--dport", fmt.Sprintf("%d", daemonPort), "-j", ingressChain); err != nil {
		return fmt.Errorf("insert ingress jump: %w", err)
	}
	if gatewayIP != "" {
		// Excluding the bridge, because a sandbox picks its own source address
		// and would otherwise reach another sandbox's daemon port by sending
		// from the gateway's. The runner dials the daemon from the host side,
		// where the packet is locally generated and never reaches a FORWARD
		// chain at all, so nothing legitimate arrives here on the bridge.
		if err := run("iptables", "-A", ingressChain, "!", "-i", bridgeIface, "-s", gatewayIP+"/32", "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("allow daemon ingress from gateway: %w", err)
		}
	}
	if err := run("iptables", "-A", ingressChain, "-j", "DROP"); err != nil {
		return fmt.Errorf("append ingress drop: %w", err)
	}

	return nil
}

// ensureBridgePolicy installs the rules that hold for every container on the
// runner bridge: egress filtering on forwarded traffic, and a block on reaching
// the runner host itself.
//
// Both match on the bridge interface rather than on a container address,
// because a sandbox controls which address it sends from and would otherwise
// only have to pick a different one to fall outside the rule. Under Sysbox the
// capability bounding set is full even for a non-root container process, so the
// sandbox image's passwordless sudo yields CAP_NET_ADMIN inside the container's
// own netns, which is enough to add an address to its interface. The interface
// a packet arrives on is not something the container can choose.
//
// The host block lives in INPUT because DOCKER-USER is only consulted for
// forwarded packets: anything addressed to a host-local address (the bridge
// gateway, the host's own private IP) is delivered locally and never reaches
// the egress chain. Everything arriving in INPUT is locally destined by
// definition, so one drop covers every host address at once. Established
// traffic is exempt, because the runner dials the sandbox daemon and the
// replies arrive here.
func ensureBridgePolicy(bridgeIface string) error {
	if bridgeIface == "" {
		return fmt.Errorf("bridge interface is required")
	}
	if strings.ContainsAny(bridgeIface, "/ ") {
		return fmt.Errorf("bridge interface %q is not a valid interface name", bridgeIface)
	}
	// iptables accepts a rule naming an interface that does not exist, and that
	// rule then matches nothing. Every sandbox would run unfiltered with no
	// error anywhere, so refuse to build the policy instead.
	if _, err := os.Stat("/sys/class/net/" + bridgeIface); err != nil {
		return fmt.Errorf("bridge interface %q not found: %w", bridgeIface, err)
	}

	if err := ensureChain(bridgeEgressChain); err != nil {
		return err
	}
	if err := ensureChain(bridgeHostChain); err != nil {
		return err
	}
	if err := resetSharedChains(); err != nil {
		return err
	}

	if err := ensureJump("DOCKER-USER", "-i", bridgeIface, "-j", bridgeEgressChain); err != nil {
		return err
	}
	if err := ensureRule(bridgeEgressChain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	for _, cidr := range netpolicy.PrivateRangesV4 {
		if err := ensureRule(bridgeEgressChain, "-d", cidr, "-j", "DROP"); err != nil {
			return err
		}
	}
	if err := ensureRule(bridgeEgressChain, "-j", "ACCEPT"); err != nil {
		return err
	}

	if err := ensureJump("INPUT", "-i", bridgeIface, "-j", bridgeHostChain); err != nil {
		return err
	}
	if err := ensureRule(bridgeHostChain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	return ensureRule(bridgeHostChain, "-j", "DROP")
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

	// The egress and host chains are no longer per container. They are still
	// torn down here so a runner upgraded in place drops the ones its previous
	// version left behind.
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

// ensureChain creates a chain unless it already exists.
func ensureChain(chain string) error {
	if err := run("iptables", "-n", "-L", chain); err == nil {
		return nil
	}
	if err := run("iptables", "-N", chain); err != nil {
		return fmt.Errorf("create chain %s: %w", chain, err)
	}
	return nil
}

// resetSharedChains empties the shared chains the first time this process
// builds them, so their contents are always the ones this binary appended.
// ensureRule only adds, so a chain inherited from an earlier binary could keep
// rules the current policy has dropped, or place new ones after the terminal
// ACCEPT, where they never match. Nothing is on the bridge while the chains are
// empty: the runtime calls EnsureBridgePolicy at startup, after it has removed
// every managed container and before it can create one, which leaves the
// rebuild from ApplyPolicy with nothing left to flush.
// Callers hold mu.
func resetSharedChains() error {
	if sharedChainsReset {
		return nil
	}
	for _, chain := range []string{bridgeEgressChain, bridgeHostChain} {
		if err := run("iptables", "-F", chain); err != nil {
			return fmt.Errorf("flush shared chain %s: %w", chain, err)
		}
	}
	sharedChainsReset = true
	return nil
}

// ensureRule appends a rule unless an identical one is already present, so that
// rebuilding the shared policy never leaves a window in which it is absent for
// the sandboxes already running under it. Appending is only in order because
// the chain started empty; see resetSharedChains.
func ensureRule(chain string, rule ...string) error {
	if err := run("iptables", append([]string{"-C", chain}, rule...)...); err == nil {
		return nil
	}
	if err := run("iptables", append([]string{"-A", chain}, rule...)...); err != nil {
		return fmt.Errorf("append rule to %s: %w", chain, err)
	}
	return nil
}

// ensureJump inserts a jump at the top of parent unless it is already present.
func ensureJump(parent string, rule ...string) error {
	if err := run("iptables", append([]string{"-C", parent}, rule...)...); err == nil {
		return nil
	}
	if err := run("iptables", append([]string{"-I", parent, "1"}, rule...)...); err != nil {
		return fmt.Errorf("insert jump into %s: %w", parent, err)
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
