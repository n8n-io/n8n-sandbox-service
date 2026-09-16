package network

import (
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

func TestForwardEgressRulesIncludesPrivateRanges(t *testing.T) {
	lines := forwardEgressRules("fc-sb-0", "fc-tap-0")
	joined := strings.Join(lines, "\n")
	for _, cidr := range netpolicy.PrivateRangesV4 {
		if !strings.Contains(joined, cidr) {
			t.Fatalf("egress rules missing %s: %s", cidr, joined)
		}
	}
	if strings.Contains(joined, "MASQUERADE") {
		t.Fatalf("forward policy should not configure NAT: %s", joined)
	}
}

// The block goes ahead of everything SetupScript appended, is keyed on the TAP
// like the rest of the policy, and is inserted only if absent.
func TestBlockEgressScriptInsertsATapKeyedDropFirst(t *testing.T) {
	script := BlockEgressScript(3, "fc-tap-0")
	for _, want := range []string{
		"set -eu",
		"ip netns exec 'fc-sb-3' iptables -w 5 -C FORWARD -i 'fc-tap-0' -j DROP",
		"|| ip netns exec 'fc-sb-3' iptables -w 5 -I FORWARD 1 -i 'fc-tap-0' -j DROP",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("block egress script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "-s ") || strings.Contains(script, "-d ") {
		t.Fatalf("block egress script must not match on addresses:\n%s", script)
	}
	if !strings.Contains(BlockEgressScript(0, ""), "'fc-tap-0'") {
		t.Fatal("empty tap device should fall back to the default")
	}
}
