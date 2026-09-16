package network

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNetnsName(t *testing.T) {
	if got := NetnsName(3); got != "fc-sb-3" {
		t.Fatalf("NetnsName(3) = %q", got)
	}
}

func TestHostVethName(t *testing.T) {
	if got := HostVethName(3); got != "fc-veth-3" {
		t.Fatalf("HostVethName(3) = %q", got)
	}
}

// Every slot up to MaxSlots must get a distinct, valid /30. Slot 256 is the one
// that used to overflow the third octet with the default capacity of 1000.
func TestUplinkSubnetCoversEverySlot(t *testing.T) {
	for _, tc := range []struct {
		slot        int
		host, netns string
	}{
		{0, "10.200.0.1", "10.200.0.2"},
		{2, "10.200.0.9", "10.200.0.10"},
		{63, "10.200.0.253", "10.200.0.254"},
		{64, "10.200.1.1", "10.200.1.2"},
		{256, "10.200.4.1", "10.200.4.2"},
		{MaxSlots - 1, "10.200.255.253", "10.200.255.254"},
	} {
		host, netns, prefix := uplinkSubnet(tc.slot)
		if host != tc.host || netns != tc.netns || prefix != "30" {
			t.Errorf("uplinkSubnet(%d) = %s %s /%s, want %s %s /30", tc.slot, host, netns, prefix, tc.host, tc.netns)
		}
	}
	seen := make(map[netip.Prefix]int, MaxSlots)
	for slot := 0; slot < MaxSlots; slot++ {
		host, netns, prefix := uplinkSubnet(slot)
		link, err := netip.ParsePrefix(host + "/" + prefix)
		if err != nil {
			t.Fatalf("uplinkSubnet(%d) host address: %v", slot, err)
		}
		peer, err := netip.ParseAddr(netns)
		if err != nil || !link.Contains(peer) {
			t.Fatalf("uplinkSubnet(%d) netns %s is not on the host link %s: %v", slot, netns, link, err)
		}
		if prev, dup := seen[link.Masked()]; dup {
			t.Fatalf("uplinkSubnet(%d) and (%d) share the link %s", slot, prev, link.Masked())
		}
		seen[link.Masked()] = slot
	}
}

func TestSetupScriptIncludesTopologyAndPolicy(t *testing.T) {
	script := SetupScript(0, "fc-tap-0", "172.16.0.1/24")
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
	script := SetupScript(3, "fc-tap-0", "172.16.0.1/24")
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

// The slot wirer builds namespaces in the background while a create may be
// building another inline, so two scripts can contend for /run/xtables.lock.
// Without -w the loser fails immediately instead of waiting; without a bound on
// it, a lock held by something stuck would stall the build for its whole budget.
func TestSetupScriptWaitsForTheXtablesLock(t *testing.T) {
	for _, line := range strings.Split(SetupScript(0, "fc-tap-0", "172.16.0.1/24"), "\n") {
		if strings.Contains(line, "iptables") && !strings.Contains(line, "iptables -w 5 ") {
			t.Errorf("iptables call does not wait (bounded) for the xtables lock: %s", line)
		}
	}
}
