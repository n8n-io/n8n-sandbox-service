package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// bridgeNetfilterPaths are the files checkBridgeNetfilter reads.
type bridgeNetfilterPaths struct {
	// callIPTables is the sysctl that makes bridged traffic traverse iptables.
	// From kernel 5.3 it is per network namespace and read from the runner's
	// own, the one the runner bridges live in.
	callIPTables string
	// osRelease holds the kernel release, shared with the host.
	osRelease string
}

var defaultBridgeNetfilterPaths = bridgeNetfilterPaths{
	callIPTables: "/proc/sys/net/bridge/bridge-nf-call-iptables",
	osRelease:    "/proc/sys/kernel/osrelease",
}

// checkBridgeNetfilter fails unless bridged traffic traverses iptables. Without
// it, traffic between sandboxes on the same bridge never reaches the netrules
// chains, and a sandbox can reach another sandbox's daemon.
//
// Before kernel 5.3 the sysctl is a single host-wide value that only exists in
// the host network namespace, so it cannot be read from here. There the check
// passes, with unverified set, only when the operator acknowledged that the
// host has br_netfilter loaded with the sysctl at 1.
func checkBridgeNetfilter(p bridgeNetfilterPaths, ackLoaded bool) (unverified bool, err error) {
	b, err := os.ReadFile(p.callIPTables)
	if errors.Is(err, fs.ErrNotExist) {
		return checkHostWideBridgeNetfilter(p, ackLoaded)
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p.callIPTables, err)
	}
	if v := strings.TrimSpace(string(b)); v != "1" {
		return false, fmt.Errorf("%s is %q, want \"1\": bridged sandbox traffic would bypass the network rules", p.callIPTables, v)
	}
	return false, nil
}

func checkHostWideBridgeNetfilter(p bridgeNetfilterPaths, ackLoaded bool) (bool, error) {
	missing := fmt.Errorf("%s does not exist: load the br_netfilter kernel module on the host", p.callIPTables)
	b, err := os.ReadFile(p.osRelease)
	if err != nil {
		return false, fmt.Errorf("%w (read kernel release: %v)", missing, err)
	}
	release := strings.TrimSpace(string(b))
	sysctlVisibleInRunner, ok := kernelAtLeast(release, 5, 3)
	if !ok {
		return false, fmt.Errorf("%w (unrecognised kernel release %q)", missing, release)
	}
	// From 5.3 the sysctl exists in every network namespace once the module is
	// loaded, so its absence means the module is not loaded.
	if sysctlVisibleInRunner {
		return false, missing
	}
	if !ackLoaded {
		return false, fmt.Errorf("kernel %s is older than 5.3, so %s is only visible on the host and cannot be checked: once br_netfilter is loaded on the host and the sysctl is 1, set %s=true", release, p.callIPTables, envAckBridgeNetfilter)
	}
	return true, nil
}

// kernelAtLeast reports whether a kernel release such as "4.4.302+" or
// "5.15.0-91-generic" is at least major.minor. ok is false when the release
// does not start with major.minor.
func kernelAtLeast(release string, major, minor int) (atLeast, ok bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false, false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, false
	}
	minorDigits := parts[1]
	if i := strings.IndexFunc(minorDigits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minorDigits = minorDigits[:i]
	}
	gotMinor, err := strconv.Atoi(minorDigits)
	if err != nil {
		return false, false
	}
	if gotMajor != major {
		return gotMajor > major, true
	}
	return gotMinor >= minor, true
}
