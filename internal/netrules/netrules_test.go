package netrules

import "testing"

func TestChainNameUsesContainerPrefix(t *testing.T) {
	got := ChainName("1234567890abcdef")
	if got != "N8N-SB-1234567890ab" {
		t.Fatalf("ChainName() = %q", got)
	}
}
