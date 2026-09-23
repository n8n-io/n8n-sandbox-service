package network

import (
	"fmt"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

// BlockEgressScript returns a script that inserts a DROP for the TAP ahead of
// the FORWARD rules SetupScript appended, keyed on the interface like those.
// Insert-if-absent, so a retried activation adds nothing. FORWARD is the only
// way out: the daemon proxy dials the guest from inside the namespace and never
// traverses it, and guest DNS goes to public resolvers, so the rule covers it.
// Teardown deletes the namespace, so there is nothing to undo.
func BlockEgressScript(slot int, tapDevice string) string {
	q := shellquote.Quote
	if tapDevice == "" {
		tapDevice = defaultTapIface
	}
	netns, tap := q(NetnsName(slot)), q(tapDevice)
	return fmt.Sprintf(`set -eu
ip netns exec %s iptables -w 5 -C FORWARD -i %s -j DROP 2>/dev/null \
  || ip netns exec %s iptables -w 5 -I FORWARD 1 -i %s -j DROP
`, netns, tap, netns, tap)
}

// forwardEgressRules returns iptables FORWARD rules applied inside a sandbox
// netns to block guest egress to private IPv4 ranges.
func forwardEgressRules(netns, tapIface string) []string {
	q := shellquote.Quote
	lines := []string{
		fmt.Sprintf("ip netns exec %s iptables -w 5 -A FORWARD -i %s -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
			q(netns), q(tapIface)),
	}
	for _, cidr := range netpolicy.PrivateRangesV4 {
		lines = append(lines, fmt.Sprintf("ip netns exec %s iptables -w 5 -A FORWARD -i %s -d %s -j DROP",
			q(netns), q(tapIface), q(cidr)))
	}
	lines = append(lines, fmt.Sprintf("ip netns exec %s iptables -w 5 -A FORWARD -i %s -j ACCEPT",
		q(netns), q(tapIface)))
	return lines
}
