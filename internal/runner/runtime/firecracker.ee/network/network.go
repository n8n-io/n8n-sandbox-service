package network

import (
	"context"
	"fmt"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

const (
	uplinkIfaceName = "fc-uplink"
	defaultTapIface = "fc-tap-0"
	guestSubnetCIDR = "172.16.0.0/24"
)

// HostVethName returns the host-side veth for a runner slot.
func HostVethName(slot int) string {
	return fmt.Sprintf("fc-veth-%d", slot)
}

func uplinkSubnet(slot int) (hostIP, netnsIP, prefix string) {
	return fmt.Sprintf("10.200.%d.1", slot), fmt.Sprintf("10.200.%d.2", slot), "24"
}

// EnsureHostNAT enables IPv4 forwarding and idempotent host NAT/forward rules
// needed for sandbox netns uplinks to reach the public internet.
func EnsureHostNAT(ctx context.Context, run func(context.Context, string, ...string) error) error {
	script := `
set -eu
sysctl -w net.ipv4.ip_forward=1
default_iface="$(ip route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -n 1)"
if [ -z "$default_iface" ]; then
  echo "no default route interface" >&2
  exit 1
fi
iptables -t nat -C POSTROUTING -o "$default_iface" -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -o "$default_iface" -j MASQUERADE
iptables -C FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
iptables -C FORWARD -i fc-veth+ -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i fc-veth+ -j ACCEPT
`
	return run(ctx, "sudo", "/bin/sh", "-c", script)
}

// SetupScript builds a shell script that creates a per-slot netns, TAP, veth
// uplink, routing, NAT, and egress policy.
func SetupScript(slot int, netnsName, tapDevice, tapCIDR string) string {
	q := shellquote.Quote
	if tapDevice == "" {
		tapDevice = defaultTapIface
	}
	hostVeth := HostVethName(slot)
	hostIP, netnsIP, prefix := uplinkSubnet(slot)

	var b strings.Builder
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "ip netns delete %s 2>/dev/null || true\n", q(netnsName))
	fmt.Fprintf(&b, "ip netns add %s\n", q(netnsName))
	fmt.Fprintf(&b, "ip link add %s type veth peer name %s netns %s\n",
		q(hostVeth), q(uplinkIfaceName), q(netnsName))
	fmt.Fprintf(&b, "ip addr add %s/%s dev %s\n", q(hostIP), prefix, q(hostVeth))
	fmt.Fprintf(&b, "ip link set %s up\n", q(hostVeth))
	fmt.Fprintf(&b, "ip netns exec %s ip addr add %s/%s dev %s\n",
		q(netnsName), q(netnsIP), prefix, q(uplinkIfaceName))
	fmt.Fprintf(&b, "ip netns exec %s ip link set %s up\n", q(netnsName), q(uplinkIfaceName))
	fmt.Fprintf(&b, "ip netns exec %s ip link set lo up\n", q(netnsName))
	fmt.Fprintf(&b, "ip netns exec %s ip tuntap add name %s mode tap\n", q(netnsName), q(tapDevice))
	fmt.Fprintf(&b, "ip netns exec %s ip addr add %s dev %s\n", q(netnsName), q(tapCIDR), q(tapDevice))
	fmt.Fprintf(&b, "ip netns exec %s ip link set %s up\n", q(netnsName), q(tapDevice))
	fmt.Fprintf(&b, "ip netns exec %s ip route add default via %s dev %s\n",
		q(netnsName), q(hostIP), q(uplinkIfaceName))
	fmt.Fprintf(&b, "ip netns exec %s sysctl -w net.ipv4.ip_forward=1\n", q(netnsName))
	fmt.Fprintf(&b, "ip netns exec %s iptables -t nat -A POSTROUTING -s %s -o %s -j MASQUERADE\n",
		q(netnsName), guestSubnetCIDR, q(uplinkIfaceName))
	for _, line := range forwardEgressRules(netnsName, tapDevice) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// CleanupScript removes host veth and the sandbox netns.
func CleanupScript(netnsName, hostVeth string) string {
	q := shellquote.Quote
	return fmt.Sprintf(`
set -eu
ip link delete %s 2>/dev/null || true
ip netns delete %s 2>/dev/null || true
`, q(hostVeth), q(netnsName))
}
