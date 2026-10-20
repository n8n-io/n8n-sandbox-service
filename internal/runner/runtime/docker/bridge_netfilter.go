package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// bridgeNFCallIPTablesPath is the sysctl that makes bridged traffic traverse
// iptables. It is per network namespace and read from the runner's own, the one
// the runner bridges live in.
const bridgeNFCallIPTablesPath = "/proc/sys/net/bridge/bridge-nf-call-iptables"

// checkBridgeNetfilter fails unless the sysctl at path reads 1. Without it,
// traffic between sandboxes on the same bridge never reaches the netrules
// chains, and a sandbox can reach another sandbox's daemon.
func checkBridgeNetfilter(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist: load the br_netfilter kernel module on the host (kernels before 5.3 also lack it outside the host network namespace)", path)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if v := strings.TrimSpace(string(b)); v != "1" {
		return fmt.Errorf("%s is %q, want \"1\": bridged sandbox traffic would bypass the network rules", path, v)
	}
	return nil
}
