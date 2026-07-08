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
