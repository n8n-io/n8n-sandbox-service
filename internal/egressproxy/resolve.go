package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

// errAddressNotAllowed is a destination that resolved into a blocked range.
var errAddressNotAllowed = errors.New("address not allowed")

// blockedV6 are the IPv6 ranges no sandbox reaches; mapped and NAT64 forms
// are checked by their embedded IPv4 address instead.
var blockedV6 = []netip.Prefix{
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("::/96"),          // IPv4-compatible
	netip.MustParsePrefix("2002::/16"),      // 6to4, which embeds any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
}

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

var blockedV4 = func() []netip.Prefix {
	out := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8")}
	for _, cidr := range netpolicy.PrivateRangesV4 {
		out = append(out, netip.MustParsePrefix(cidr))
	}
	return out
}()

// blocked reports whether addr is in a range no sandbox reaches, or is one of
// the runner's own addresses: the proxy dials from the runner's network
// namespace, where those reach the host's own services.
func blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if localAddrs.has(addr) {
		return true
	}
	if addr.Is6() && nat64.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	ranges := blockedV4
	if addr.Is6() {
		ranges = blockedV6
	}
	for _, p := range ranges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Resolver looks names up for the proxy.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// resolveChecked resolves host as an absolute name, with no search domains,
// and returns one address to dial. Every record is checked: a name with any
// blocked address is refused unless internal, so a rebinding answer cannot
// slip one past. The caller dials the returned address and never resolves
// again.
func resolveChecked(ctx context.Context, r Resolver, host string, internal bool) (netip.Addr, error) {
	name := strings.TrimSuffix(host, ".") + "."
	addrs, err := r.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("resolve %s: no addresses", host)
	}
	if !internal {
		for _, a := range addrs {
			if blocked(a) {
				return netip.Addr{}, fmt.Errorf("%s resolves to %s: %w", host, a, errAddressNotAllowed)
			}
		}
	}
	// Prefer IPv4: sandbox networks are IPv4, and so are most internal services.
	for _, a := range addrs {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
	}
	return addrs[0], nil
}

// netResolver is the system resolver.
type netResolver struct{}

func (netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// localAddrs caches the runner's interface addresses, refreshed at most every
// few seconds, so an address the host gains is refused soon after.
var localAddrs = &localAddrCache{}

type localAddrCache struct {
	mu    sync.Mutex
	at    time.Time
	addrs map[netip.Addr]bool
}

const localAddrRefresh = 5 * time.Second

func (c *localAddrCache) has(a netip.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.addrs == nil || time.Since(c.at) > localAddrRefresh {
		addrs := map[netip.Addr]bool{}
		if ifAddrs, err := net.InterfaceAddrs(); err == nil {
			for _, ia := range ifAddrs {
				if p, err := netip.ParsePrefix(ia.String()); err == nil {
					addrs[p.Addr().Unmap()] = true
				}
			}
		}
		c.addrs, c.at = addrs, time.Now()
	}
	return c.addrs[a]
}
