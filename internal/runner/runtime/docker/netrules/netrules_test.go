package netrules

import (
	"reflect"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

func TestChainNameUsesContainerPrefix(t *testing.T) {
	got := ChainName("1234567890abcdef")
	if got != "N8N-SB-1234567890ab" {
		t.Fatalf("ChainName() = %q", got)
	}
}

// ensureRule only appends, so this order is the order in the chain: the bridge
// drop ahead of everything, every blocked range, and the terminal accept last,
// where nothing can land behind it.
func TestBridgeEgressRulesOrder(t *testing.T) {
	got := bridgeEgressRules("runner-bridge")

	want := [][]string{
		{"-o", "runner-bridge", "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}
	for _, cidr := range netpolicy.PrivateRangesV4 {
		want = append(want, []string{"-d", cidr, "-j", "DROP"})
	}
	want = append(want, []string{"-j", "ACCEPT"})

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bridgeEgressRules() =\n%v\nwant\n%v", got, want)
	}
}
