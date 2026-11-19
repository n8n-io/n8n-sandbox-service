package egressproxy

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/gitfilter"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
)

// The runner's own addresses, and IPv6 forms that embed or translate IPv4,
// are refused however public they look.
func TestBlockedAddresses(t *testing.T) {
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, ia := range ifAddrs {
		if p, err := netip.ParsePrefix(ia.String()); err == nil && !blocked(p.Addr()) {
			t.Errorf("runner address %s not blocked", p.Addr())
		}
	}
	for addr, want := range map[string]bool{
		"93.184.215.14":         false,
		"2606:2800:21f:cb07::1": false,
		"10.1.2.3":              true,
		"::ffff:10.1.2.3":       true,
		"64:ff9b::a01:203":      true, // NAT64 of 10.1.2.3
		"64:ff9b:1::1":          true,
		"2002:a01:203::1":       true, // 6to4
		"::a01:203":             true, // IPv4-compatible
		"fd00::1":               true,
		"169.254.169.254":       true,
	} {
		if got := blocked(netip.MustParseAddr(addr)); got != want {
			t.Errorf("blocked(%s) = %v, want %v", addr, got, want)
		}
	}
}

// A command list that would hold more than the bound in memory is refused.
func TestPushHeadIsBounded(t *testing.T) {
	// Over 1 MiB of command list: 20 packets of 65 000 bytes.
	var over strings.Builder
	payload := strings.Repeat("z", 65000)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&over, "%04x%s", len(payload)+4, payload)
	}
	if _, err := gitfilter.ReadCommands(bufio.NewReader(strings.NewReader(over.String())), []string{"refs/heads/factory/i1/"}); err == nil {
		t.Fatal("oversized command list accepted")
	}
}

func TestBackgroundResponsesRefused(t *testing.T) {
	for body, want := range map[string]bool{
		`{"model":"gpt-6","input":"hi"}`:                    false,
		`{"model":"gpt-6","input":"hi","background":false}`: false,
		`{"model":"gpt-6","input":"hi","background":true}`:  true,
		`{"model":`: true,
	} {
		if got := metering.RequestsBackground([]byte(body)); got != want {
			t.Errorf("%s: %v", body, got)
		}
	}
}

// FuzzParseAuthority: whatever a guest sends as a destination, an accepted
// one is a lowercase DNS name with a real port, never an IP literal.
func FuzzParseAuthority(f *testing.F) {
	for _, s := range []string{"example.com:443", "API.example.com", "10.0.0.1:80", "[::1]:443", "a..b", "x.test:0443"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		host, port, ok := parseAuthority(s, 443)
		if !ok {
			return
		}
		if port < 1 || port > 65535 || host != strings.ToLower(host) || net.ParseIP(host) != nil {
			t.Fatalf("accepted %q as %q:%d", s, host, port)
		}
		for _, c := range host {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
				t.Fatalf("accepted %q with %q", s, c)
			}
		}
	})
}
