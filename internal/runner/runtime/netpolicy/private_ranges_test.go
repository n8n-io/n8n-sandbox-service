package netpolicy

import (
	"net"
	"slices"
	"testing"
)

// iptables accepts a CIDR with host bits set and silently masks them, so an
// entry that is not in canonical form blocks something other than it says.
func TestPrivateRangesAreCanonicalCIDRs(t *testing.T) {
	for _, cidr := range PrivateRangesV4 {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("%q is not a CIDR: %v", cidr, err)
		}
		if ip.To4() == nil {
			t.Fatalf("%q is not IPv4", cidr)
		}
		if network.String() != cidr {
			t.Fatalf("%q is not canonical, want %q", cidr, network.String())
		}
	}
}

func TestPrivateRangesBlockPlatformAndMulticast(t *testing.T) {
	for _, want := range []string{
		"169.254.0.0/16",   // instance metadata service
		"168.63.129.16/32", // Azure WireServer and platform DNS
		"224.0.0.0/4",      // multicast
	} {
		if !slices.Contains(PrivateRangesV4, want) {
			t.Errorf("PrivateRangesV4 is missing %s", want)
		}
	}
}

// The resolvers scripts/firecracker.ee/build-rootfs-template.sh seeds into the
// guest. Firecracker guests reach them through the denylist, so blocking one
// breaks name resolution in every guest.
func TestFirecrackerGuestResolversAreNotBlocked(t *testing.T) {
	for _, resolver := range []string{"8.8.8.8", "1.1.1.1"} {
		ip := net.ParseIP(resolver)
		if ip == nil || ip.To4() == nil {
			t.Fatalf("resolver %q is not an IPv4 address", resolver)
		}
		for _, cidr := range PrivateRangesV4 {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatalf("%q is not a CIDR: %v", cidr, err)
			}
			if network.Contains(ip) {
				t.Errorf("resolver %s is blocked by %s", resolver, cidr)
			}
		}
	}
}
