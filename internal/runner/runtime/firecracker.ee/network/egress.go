package network

import (
	"fmt"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

// forwardEgressRules returns iptables FORWARD rules applied inside a sandbox
// netns to block guest egress to private IPv4 ranges.
func forwardEgressRules(netns, tapIface string) []string {
	q := shellquote.Quote
	lines := []string{
		fmt.Sprintf("ip netns exec %s iptables -A FORWARD -i %s -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
			q(netns), q(tapIface)),
	}
	for _, cidr := range netpolicy.PrivateRangesV4 {
		lines = append(lines, fmt.Sprintf("ip netns exec %s iptables -A FORWARD -i %s -d %s -j DROP",
			q(netns), q(tapIface), q(cidr)))
	}
	lines = append(lines, fmt.Sprintf("ip netns exec %s iptables -A FORWARD -i %s -j ACCEPT",
		q(netns), q(tapIface)))
	return lines
}
