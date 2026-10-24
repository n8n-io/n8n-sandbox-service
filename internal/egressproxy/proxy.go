// Package egressproxy is the runner's egress proxy for sandboxes created with
// egress "proxy". A guest reaches nothing else: its traffic leaves only
// through this proxy, which allows the destinations its spec lists, inspects
// those that carry repositories, credentials or model APIs, injects
// credentials the guest never sees, and meters model usage.
package egressproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/credentials"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Refusal reasons, sent in X-Sandbox-Egress-Refusal and the body.
const (
	ReasonHostNotAllowed        = "host_not_allowed"
	ReasonAddressNotAllowed     = "address_not_allowed"
	ReasonPathNotAllowed        = "path_not_allowed"
	ReasonRepoNotAllowed        = "repo_not_allowed"
	ReasonRefNotAllowed         = "ref_not_allowed"
	ReasonModelBlocked          = "model_blocked"
	ReasonCredentialUnavailable = "credential_unavailable"
)

// RefusalHeader carries the reason of every refusal.
const RefusalHeader = "X-Sandbox-Egress-Refusal"

// Config is what a Server needs from the runner.
type Config struct {
	// CA signs the certificates presented for inspected hosts.
	CA *CA
	// InternalHosts may resolve into the blocked ranges (the Git service).
	InternalHosts map[string]bool
	// UpstreamRoots verifies upstream TLS; nil is the system pool.
	UpstreamRoots *x509.CertPool
	// Secrets resolves credentials; nil makes every credential unavailable.
	Secrets *credentials.Store
	// Sink receives usage records; nil drops them.
	Sink *metering.Sink

	// Resolver and Dial are replaced by tests.
	Resolver Resolver
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Server holds every proxy sandbox's policy on this runner.
type Server struct {
	cfg Config

	mu        sync.RWMutex
	sandboxes map[string]*sandbox

	transports     sync.Map // transportKey -> *http.Transport
	transportCount atomic.Int32
}

// Per-sandbox bounds on what one guest holds open on the runner.
const (
	maxConnsPerSandbox = 256
	tunnelIdleTimeout  = 10 * time.Minute
)

type sandbox struct {
	id, tenant string
	policy     atomic.Pointer[policy]
	blocked    atomic.Bool
	conns      atomic.Int32

	mu      sync.Mutex
	tunnels map[net.Conn]target // open tunnels and where they go
}

// New returns a Server with no sandboxes.
func New(cfg Config) *Server {
	if cfg.Resolver == nil {
		cfg.Resolver = netResolver{}
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		cfg.Dial = d.DialContext
	}
	return &Server{cfg: cfg, sandboxes: map[string]*sandbox{}}
}

// StartSink delivers usage records until ctx ends.
func (s *Server) StartSink(ctx context.Context) {
	if s.cfg.Sink != nil {
		go s.cfg.Sink.Run(ctx)
	}
}

// CACertPEM is the certificate guests must trust.
func (s *Server) CACertPEM() []byte { return s.cfg.CA.CertPEM() }

// HasSink reports whether usage can be delivered; without it a spec naming
// model hosts is refused.
func (s *Server) HasSink() bool { return s.cfg.Sink != nil }

// Set installs or replaces a sandbox's policy. It applies to every request
// received after it returns; open tunnels to destinations no longer allowed
// are closed.
func (s *Server) Set(sandboxID, tenant string, spec *runnerruntime.EgressProxySpec) {
	p := compile(spec)
	s.mu.Lock()
	sb, ok := s.sandboxes[sandboxID]
	if !ok {
		sb = &sandbox{id: sandboxID, tenant: tenant, tunnels: map[net.Conn]target{}}
		s.sandboxes[sandboxID] = sb
	}
	s.mu.Unlock()
	sb.policy.Store(p)
	sb.mu.Lock()
	for conn, t := range sb.tunnels {
		// A host that became inspected must not keep a tunnel nothing sees into.
		if !p.permits(t) || p.inspected[t.key] {
			_ = conn.Close()
		}
	}
	sb.mu.Unlock()
}

// Tenant returns the owner of a proxy sandbox.
func (s *Server) Tenant(sandboxID string) (string, bool) {
	sb := s.lookup(sandboxID)
	if sb == nil {
		return "", false
	}
	return sb.tenant, true
}

// Has reports whether sandboxID is a proxy sandbox on this runner.
func (s *Server) Has(sandboxID string) bool { return s.lookup(sandboxID) != nil }

// HasSecret reports whether tenant's secret ref can be read now.
func (s *Server) HasSecret(tenant, ref string) bool {
	if s.cfg.Secrets == nil {
		return false
	}
	_, err := s.cfg.Secrets.Secret(tenant, ref)
	return err == nil
}

// SetModelBlocked blocks or unblocks new requests to the sandbox's model hosts.
func (s *Server) SetModelBlocked(sandboxID string, blocked bool) bool {
	sb := s.lookup(sandboxID)
	if sb == nil {
		return false
	}
	sb.blocked.Store(blocked)
	return true
}

// Remove forgets a sandbox and closes its tunnels.
func (s *Server) Remove(sandboxID string) {
	s.mu.Lock()
	sb := s.sandboxes[sandboxID]
	delete(s.sandboxes, sandboxID)
	s.mu.Unlock()
	if sb == nil {
		return
	}
	sb.policy.Store(&policy{})
	sb.mu.Lock()
	for conn := range sb.tunnels {
		_ = conn.Close()
	}
	sb.mu.Unlock()
}

func (s *Server) lookup(id string) *sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sandboxes[id]
}

// Handler serves the proxy on a listener the guests reach; identify maps a
// connection's remote address to the sandbox it comes from.
func (s *Server) Handler(identify func(remoteAddr string) (string, bool)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identify(r.RemoteAddr)
		var sb *sandbox
		if ok {
			sb = s.lookup(id)
		}
		if sb == nil {
			refuse(w, http.StatusForbidden, ReasonHostNotAllowed, "", "no egress policy for this sandbox")
			return
		}
		if sb.conns.Add(1) > maxConnsPerSandbox {
			sb.conns.Add(-1)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many proxy connections from this sandbox", http.StatusTooManyRequests)
			return
		}
		defer sb.conns.Add(-1)
		switch {
		case r.Method == http.MethodConnect:
			s.handleConnect(w, r, sb)
		case r.URL.IsAbs() && r.URL.Scheme == "http":
			s.handlePlain(w, r, sb)
		default:
			refuse(w, http.StatusBadRequest, ReasonHostNotAllowed, "", "use the proxy with CONNECT or an absolute http URL")
		}
	})
}

// target is one destination a connection or request goes to.
type target struct {
	host   string
	port   int
	key    string
	scheme string
	addr   netip.Addr
}

func (t target) authority() string { return net.JoinHostPort(t.host, strconv.Itoa(t.port)) }

// resolveTarget checks a destination against the policy and resolves it.
func (s *Server) resolveTarget(ctx context.Context, sb *sandbox, host string, port, defaultPort int, scheme string) (target, string, error) {
	t := target{host: host, port: port, key: destKey(host, port, defaultPort), scheme: scheme}
	p := sb.policy.Load()
	if !p.allows(t.key) {
		return t, ReasonHostNotAllowed, errors.New("destination not allowed")
	}
	// An internal host is exempt from the address check only when the spec
	// names it: allow_public never reaches one.
	addr, err := resolveChecked(ctx, s.cfg.Resolver, host, s.cfg.InternalHosts[host] && p.allow[t.key])
	if errors.Is(err, errAddressNotAllowed) {
		return t, ReasonAddressNotAllowed, err
	}
	if err != nil {
		return t, "", err
	}
	t.addr = addr
	return t, "", nil
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	host, port, ok := parseAuthority(r.Host, 443)
	if !ok {
		s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonHostNotAllowed, "", r.Host)
		return
	}
	t, reason, err := s.resolveTarget(r.Context(), sb, host, port, 443, "https")
	if err != nil {
		if reason != "" {
			s.refuseLogged(w, r, sb, http.StatusForbidden, reason, "", t.key)
		} else {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		return
	}
	rc := http.NewResponseController(w)
	conn, brw, err := rc.Hijack()
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = conn.Close()
		return
	}
	// Bytes the guest sent ahead of the 200 belong to the tunnel.
	guest := &bufferedConn{Conn: conn, r: brw.Reader}
	if sb.policy.Load().inspected[t.key] {
		s.serveInspected(guest, sb, t)
		return
	}
	s.tunnel(guest, sb, t)
}

// tunnel joins the guest to the destination without looking inside. It is
// registered before the dial, and the policy checked again after, so a spec
// replaced meanwhile cannot miss it.
func (s *Server) tunnel(guest net.Conn, sb *sandbox, t target) {
	defer guest.Close()
	sb.mu.Lock()
	sb.tunnels[guest] = t
	sb.mu.Unlock()
	defer func() {
		sb.mu.Lock()
		delete(sb.tunnels, guest)
		sb.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	upstream, err := s.cfg.Dial(ctx, "tcp", net.JoinHostPort(t.addr.String(), strconv.Itoa(t.port)))
	cancel()
	if err != nil {
		return
	}
	defer upstream.Close()
	if p := sb.policy.Load(); !p.permits(t) || p.inspected[t.key] {
		return
	}
	g, u := &idleConn{Conn: guest}, &idleConn{Conn: upstream}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(u, g); done <- struct{}{} }()
	go func() { _, _ = io.Copy(g, u); done <- struct{}{} }()
	<-done
}

// idleConn ends a tunnel that carries nothing for tunnelIdleTimeout.
type idleConn struct{ net.Conn }

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(tunnelIdleTimeout))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(tunnelIdleTimeout))
	return c.Conn.Write(b)
}

// serveInspected terminates the guest's TLS with a certificate for the CONNECT
// target, whatever SNI says, and serves its requests one by one.
func (s *Server) serveInspected(guest net.Conn, sb *sandbox, t target) {
	leaf, err := s.cfg.CA.leaf(t.host)
	if err != nil {
		_ = guest.Close()
		return
	}
	tlsConn := tls.Server(guest, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	srv := &http.Server{
		Handler:           s.requestHandler(sb, t),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// HTTP/1.1 only: no h2 frames to filter per stream.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:     nil,
	}
	_ = srv.Serve(&oneConnListener{conn: tlsConn})
}

// handlePlain serves an absolute-form plain HTTP request.
func (s *Server) handlePlain(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	host, port, ok := parseAuthority(r.URL.Host, 80)
	if !ok {
		s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonHostNotAllowed, "", r.URL.Host)
		return
	}
	t, reason, err := s.resolveTarget(r.Context(), sb, host, port, 80, "http")
	if err != nil {
		if reason != "" {
			s.refuseLogged(w, r, sb, http.StatusForbidden, reason, "", t.key)
		} else {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		return
	}
	s.requestHandler(sb, t).ServeHTTP(w, r)
}

// bufferedConn reads first what was buffered before a hijack.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// oneConnListener hands one connection to http.Server, then blocks until it
// is closed.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	done := l.done
	l.mu.Unlock()
	var c net.Conn
	l.once.Do(func() { c = &closeNotifyConn{Conn: l.conn, done: done} })
	if c != nil {
		return c, nil
	}
	<-done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotifyConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// refuseLogged refuses and logs with the sandbox's attribution, never with a
// credential or a query string.
func (s *Server) refuseLogged(w http.ResponseWriter, r *http.Request, sb *sandbox, status int, reason string, provider metering.Provider, dest string) {
	p := sb.policy.Load()
	slog.Warn("egress refused",
		"sandbox_id", sb.id, "tenant_id", sb.tenant, "labels", p.labels,
		"reason", reason, "destination", dest, "method", r.Method, "path", r.URL.Path)
	refuse(w, status, reason, provider, "")
}

// refuse writes a refusal: in the provider's error shape for a model host, so
// a CLI shows it, otherwise the proxy's own.
func refuse(w http.ResponseWriter, status int, reason string, provider metering.Provider, msg string) {
	if msg == "" {
		msg = "egress refused: " + reason
	}
	w.Header().Set(RefusalHeader, reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body string
	switch provider {
	case metering.Anthropic:
		body = `{"type":"error","error":{"type":"permission_error","message":` + strconv.Quote(msg) + `}}`
	case metering.OpenAI:
		body = `{"error":{"type":"permission_error","code":` + strconv.Quote(reason) + `,"message":` + strconv.Quote(msg) + `}}`
	default:
		body = `{"error":` + strconv.Quote(msg) + `,"reason":` + strconv.Quote(reason) + `}`
	}
	_, _ = io.WriteString(w, body+"\n")
}
