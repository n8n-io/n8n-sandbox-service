package network

import (
	"strings"
	"testing"
)

func TestHostVethName(t *testing.T) {
	if got := HostVethName(3); got != "fc-veth-3" {
		t.Fatalf("HostVethName(3) = %q", got)
	}
}

func TestUplinkSubnet(t *testing.T) {
	host, netns, prefix := uplinkSubnet(2)
	if host != "10.200.2.1" || netns != "10.200.2.2" || prefix != "24" {
		t.Fatalf("uplinkSubnet(2) = %s %s %s", host, netns, prefix)
	}
}

func TestSetupScriptIncludesTopologyAndPolicy(t *testing.T) {
	script := SetupScript(0, "fc-sb-0", "fc-tap-0", "172.16.0.1/24")
	for _, want := range []string{"fc-veth-0", "fc-uplink", "172.16.0.0/12", "MASQUERADE"} {
		if !strings.Contains(script, want) {
			t.Fatalf("setup script missing %q", want)
		}
	}
}

// A slot can be reused after a teardown that failed before removing anything, so
// setup has to clear both of the per-slot host names it goes on to create. Missing
// either one fails the whole script under `set -eu` and strands the slot.
func TestSetupScriptClearsSlotBeforeCreatingIt(t *testing.T) {
	script := SetupScript(3, "fc-sb-3", "fc-tap-0", "172.16.0.1/24")
	for _, want := range []string{
		"ip link delete 'fc-veth-3' 2>/dev/null || true",
		"ip netns delete 'fc-sb-3' 2>/dev/null || true",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("setup script missing %q, so a leftover from a failed teardown breaks slot reuse", want)
		}
	}
	if strings.Index(script, "ip link delete 'fc-veth-3'") > strings.Index(script, "ip link add 'fc-veth-3'") {
		t.Error("setup script deletes the host veth after creating it")
	}
}
