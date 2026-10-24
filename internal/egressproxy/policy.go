package egressproxy

import (
	"net"
	"strconv"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/gitfilter"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// policy is a validated EgressProxySpec, indexed by destination key: a host
// as the spec writes it, "name" for the default port or "name:port".
type policy struct {
	allow     map[string]bool
	public    bool // allow_public
	inspected map[string]bool
	repos     map[string][]gitfilter.Repo
	pushRefs  []string
	creds     map[string]runnerruntime.EgressCredential
	models    map[string]metering.Provider
	labels    map[string]string
}

func compile(spec *runnerruntime.EgressProxySpec) *policy {
	p := &policy{
		allow:     map[string]bool{},
		inspected: map[string]bool{},
		repos:     map[string][]gitfilter.Repo{},
		pushRefs:  spec.PushRefs,
		creds:     map[string]runnerruntime.EgressCredential{},
		models:    map[string]metering.Provider{},
		labels:    spec.Labels,
		public:    spec.AllowPublic,
	}
	for _, e := range spec.Allow {
		for _, h := range e.Hosts {
			p.allow[h] = true
		}
	}
	for _, r := range spec.Repos {
		p.repos[r.Host] = append(p.repos[r.Host], gitfilter.Repo{Path: r.Path, Write: r.Access == runnerruntime.RepoWrite})
		p.inspected[r.Host] = true
	}
	for _, c := range spec.Credentials {
		p.creds[c.Host] = c
		p.inspected[c.Host] = true
	}
	for _, m := range spec.ModelHosts {
		p.models[m.Host] = metering.Provider(m.Provider)
		p.inspected[m.Host] = true
	}
	return p
}

// allows reports whether the policy lets key through: a listed destination,
// or with allow_public any host on its scheme's default port, which is the
// only key with no port.
func (p *policy) allows(key string) bool {
	return p.allow[key] || (p.public && !strings.Contains(key, ":"))
}

// permits reports whether the policy still lets an open connection to t
// through. Through allow_public that needs a public address too: a host the
// spec no longer names may have been reached at an internal one.
func (p *policy) permits(t target) bool {
	return p.allow[t.key] || (p.allows(t.key) && !blocked(t.addr))
}

// destKey is the policy key of host:port, where defaultPort is the scheme's.
func destKey(host string, port, defaultPort int) string {
	if port == defaultPort {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

// parseAuthority splits an authority into a lowercase host and a port,
// defaultPort when absent. IP literals and anything not a DNS name fail: the
// allow list holds names only.
func parseAuthority(authority string, defaultPort int) (string, int, bool) {
	host, portStr, err := net.SplitHostPort(authority)
	if err != nil {
		host, portStr = authority, ""
	}
	port := defaultPort
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != portStr {
			return "", 0, false
		}
		port = n
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return "", 0, false
	}
	for _, c := range host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return "", 0, false
		}
	}
	return host, port, true
}
