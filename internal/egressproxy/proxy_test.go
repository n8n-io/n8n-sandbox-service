package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/credentials"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// fakeResolver answers from a table; every test host lives on 127.0.0.1, so
// the ones a test reaches are listed as internal.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[strings.TrimSuffix(host, ".")]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

type harness struct {
	t      *testing.T
	srv    *Server
	proxy  *httptest.Server
	ca     *CA
	guest  *http.Client // trusts the proxy CA only
	sinkMu sync.Mutex
	sunk   []metering.Record
}

// newHarness starts a proxy for one sandbox, "sb", with spec.
func newHarness(t *testing.T, spec string, upstreamRoots *x509.CertPool, internal ...string) *harness {
	t.Helper()
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ca: ca}
	sinkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Records []metering.Record }
		_ = json.NewDecoder(r.Body).Decode(&b)
		h.sinkMu.Lock()
		h.sunk = append(h.sunk, b.Records...)
		h.sinkMu.Unlock()
	}))
	t.Cleanup(sinkSrv.Close)
	secretsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secretsDir, "tenant-1", "factory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"git-token": "git-secret", "anthropic-key": "sk-ant-secret"} {
		if err := os.WriteFile(filepath.Join(secretsDir, "tenant-1", "factory", name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := credentials.Open(secretsDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	res := fakeResolver{
		"internal-only.test": {netip.MustParseAddr("10.0.0.5")},
		"rebind.test":        {netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("169.254.169.254")},
		"mapped.test":        {netip.MustParseAddr("::ffff:127.0.0.1")},
	}
	internalHosts := map[string]bool{}
	for _, name := range internal {
		res[name] = loopback
		internalHosts[name] = true
	}
	res["loopback.test"] = loopback
	h.srv = New(Config{
		CA: ca, InternalHosts: internalHosts, UpstreamRoots: upstreamRoots, Secrets: store,
		Sink: metering.NewSink(sinkSrv.URL, "tok", "runner-1"), Resolver: res,
	})
	h.set(spec)
	h.proxy = httptest.NewServer(h.srv.Handler(func(string) (string, bool) { return "sb", true }))
	t.Cleanup(h.proxy.Close)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	proxyURL, _ := url.Parse(h.proxy.URL)
	h.guest = &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return h
}

func (h *harness) set(spec string) {
	h.t.Helper()
	opts, err := runnerruntime.ParseCreateOptions(`{"egress":"proxy","tenant":"tenant-1","egress_proxy":` + spec + `}`)
	if err != nil {
		h.t.Fatalf("spec: %v", err)
	}
	h.srv.Set("sb", "tenant-1", opts.EgressProxy)
}

func (h *harness) flushSink() []metering.Record {
	for h.srv.cfg.Sink.Flush(context.Background()) {
	}
	h.sinkMu.Lock()
	defer h.sinkMu.Unlock()
	return append([]metering.Record(nil), h.sunk...)
}

func hostPort(t *testing.T, rawURL string) (string, string) {
	t.Helper()
	u, _ := url.Parse(rawURL)
	_, port, _ := net.SplitHostPort(u.Host)
	return u.Host, port
}

func expectRefusal(t *testing.T, resp *http.Response, err error, status int, reason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status || resp.Header.Get(RefusalHeader) != reason {
		t.Fatalf("got %d %q %s, want %d %s", resp.StatusCode, resp.Header.Get(RefusalHeader), body, status, reason)
	}
}

// Plain HTTP to an allowed host goes through; any other host, an IP literal,
// a blocked address and a rebinding name do not.
func TestPlainHTTPAllowList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.Host)
	}))
	defer up.Close()
	_, port := hostPort(t, up.URL)
	h := newHarness(t, `{"allow":[{"name":"svc","hosts":["svc.test:`+port+`","internal-only.test","rebind.test","mapped.test"]}]}`, nil, "svc.test")

	resp, err := h.guest.Get("http://svc.test:" + port + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello svc.test:"+port {
		t.Fatalf("allowed: %d %s", resp.StatusCode, body)
	}

	resp, err = h.guest.Get("http://other.test:" + port + "/")
	expectRefusal(t, resp, err, 403, ReasonHostNotAllowed)
	resp, err = h.guest.Get("http://127.0.0.1:" + port + "/")
	expectRefusal(t, resp, err, 403, ReasonHostNotAllowed)
	resp, err = h.guest.Get("http://internal-only.test/")
	expectRefusal(t, resp, err, 403, ReasonAddressNotAllowed)
	resp, err = h.guest.Get("http://rebind.test/")
	expectRefusal(t, resp, err, 403, ReasonAddressNotAllowed)
	resp, err = h.guest.Get("http://mapped.test/")
	expectRefusal(t, resp, err, 403, ReasonAddressNotAllowed)
}

// A host with no repos, credentials or model API is tunnelled untouched: the
// guest sees the upstream's own certificate.
func TestConnectTunnelsUninspectedHost(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnelled")
	}))
	defer up.Close()
	_, port := hostPort(t, up.URL)
	h := newHarness(t, `{"allow":[{"name":"svc","hosts":["svc.test:`+port+`"]}]}`, nil, "svc.test")
	proxyURL, _ := url.Parse(h.proxy.URL)
	pool := x509.NewCertPool()
	pool.AddCert(up.Certificate())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}}
	resp, err := client.Get("https://svc.test:" + port + "/")
	if err != nil {
		t.Fatalf("tunnel: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tunnelled" {
		t.Fatalf("body %q", body)
	}
	resp, err = client.Get("https://svc.test:1/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("CONNECT to an unlisted port went through")
	}
}

// An inspected host: TLS is terminated with the runner CA, the credential is
// injected over whatever the guest sent, the guest never sees it, a request
// for another Host on the connection is refused, and redirects and upgrades
// are not followed.
func TestInspectedHostInjectsCredential(t *testing.T) {
	// Nothing the proxy logs may carry the secret.
	var logs bytes.Buffer
	defer func(l *slog.Logger) { slog.SetDefault(l) }(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var gotAuth, gotRange []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		gotRange = append(gotRange, r.Header.Get("Range"))
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://evil.test/steal", http.StatusFound)
			return
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
		case "/second-encoding":
			w.Header()["Content-Encoding"] = []string{"identity", "gzip"}
		case "/partial":
			w.Header().Set("X-Echo", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "ret")
			return
		}
		// An upstream that echoes the request's credential, as a debug
		// endpoint or an error page might.
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "you sent: "+r.Header.Get("Authorization"))
	}))
	defer up.Close()
	_, port := hostPort(t, up.URL)
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	host := "example.com:" + port // httptest certificates name example.com
	h := newHarness(t, `{"allow":[{"name":"api","hosts":["`+host+`"]}],
		"credentials":[{"host":"`+host+`","secret_ref":"factory/git-token"}]}`, roots, "example.com")

	req, _ := http.NewRequest("GET", "https://"+host+"/x", nil)
	req.Header.Set("Authorization", "Bearer guest-made-up")
	resp, err := h.guest.Do(req)
	if err != nil {
		t.Fatalf("inspected request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(gotAuth) != 1 || gotAuth[0] != "Bearer git-secret" {
		t.Fatalf("status %d upstream auth %v", resp.StatusCode, gotAuth)
	}
	if strings.Contains(string(body), "git-secret") || strings.Contains(resp.Header.Get("X-Echo"), "git-secret") {
		t.Fatalf("secret in the response: %q %q", body, resp.Header.Get("X-Echo"))
	}
	if !strings.HasPrefix(string(body), "you sent: Bearer ***") {
		t.Fatalf("echo not masked in place: %q", body)
	}
	// A response the proxy cannot scrub whole is refused: compressed in any
	// listed encoding, or a range, which could split the secret past the mask.
	for _, path := range []string{"/gzip", "/second-encoding", "/partial"} {
		req, _ := http.NewRequest("GET", "https://"+host+path, nil)
		req.Header.Set("Range", "bytes=0-2")
		resp, err = h.guest.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "git-secret") {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
	for _, rg := range gotRange {
		if rg != "" {
			t.Fatalf("Range reached the credentialed host: %q", gotRange)
		}
	}

	resp, err = h.guest.Get("https://" + host + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://evil.test/steal" {
		t.Fatalf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	req, _ = http.NewRequest("GET", "https://"+host+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err = h.guest.Do(req)
	expectRefusal(t, resp, err, 403, ReasonPathNotAllowed)

	// Inside the inspected tunnel, a request naming another host is refused
	// rather than routed to it.
	if got := rawInspectedRequest(t, h, host, "other.test"); !strings.Contains(got, RefusalHeader+": "+ReasonHostNotAllowed) {
		t.Fatalf("other Host inside the tunnel: %q", got)
	}
	if strings.Contains(logs.String(), "git-secret") {
		t.Fatalf("secret logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "egress refused") {
		t.Fatal("refusals were not logged, so the log check proves nothing")
	}
}

// rawInspectedRequest opens a CONNECT tunnel to dest and sends one request
// with the given Host header, returning the raw response head.
func rawInspectedRequest(t *testing.T, h *harness, dest, hostHeader string) string {
	t.Helper()
	proxyAddr := strings.TrimPrefix(h.proxy.URL, "http://")
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dest, dest)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	// SNI names a third host: the proxy ignores it.
	tc := tls.Client(&bufferedConn{Conn: c, r: br}, &tls.Config{RootCAs: pool, ServerName: "example.com", NextProtos: []string{"http/1.1"}})
	fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostHeader)
	raw, _ := io.ReadAll(tc)
	return string(raw)
}

// A replaced spec applies to the next request, and closes tunnels to hosts it
// no longer allows.
func TestSetClosesTunnelsNoLongerAllowed(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c) }()
		}
	}()
	_, port, _ := net.SplitHostPort(up.Addr().String())
	h := newHarness(t, `{"allow":[{"name":"echo","hosts":["echo.test:`+port+`"]}]}`, nil, "echo.test")
	c, err := net.Dial("tcp", strings.TrimPrefix(h.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT echo.test:%s HTTP/1.1\r\nHost: echo.test:%s\r\n\r\n", port, port)
	br := bufio.NewReader(c)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v", err)
	}
	fmt.Fprint(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo %q", line)
	}
	h.set(`{"allow":[]}`)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("tunnel survived the spec that dropped its host")
	}
}

// Model hosts: only listed paths, blocked on demand, metered from the stream,
// and refusals in the provider's error shape.
func TestModelHostMeteringAndBlock(t *testing.T) {
	var gotKey, gotEncoding string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotEncoding = r.Header.Get("X-Api-Key"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_123")
		for _, ev := range []string{
			`{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":500,"cache_creation_input_tokens":20}}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"message_delta","usage":{"output_tokens":42}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()
	_, port := hostPort(t, up.URL)
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	host := "example.com:" + port
	h := newHarness(t, `{"allow":[{"name":"anthropic","hosts":["`+host+`"]}],
		"credentials":[{"host":"`+host+`","secret_ref":"factory/anthropic-key","scheme":"header","header":"x-api-key"}],
		"model_hosts":[{"host":"`+host+`","provider":"anthropic"}],
		"labels":{"instance_id":"i1","session_id":"s1"}}`, roots, "example.com")

	req, _ := http.NewRequest("POST", "https://"+host+"/v1/messages?beta=true", strings.NewReader(`{"model":"claude-opus-5-5","stream":true}`))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.guest.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "message_delta") || gotKey != "sk-ant-secret" || gotEncoding != "" {
		t.Fatalf("status %d key %q encoding %q", resp.StatusCode, gotKey, gotEncoding)
	}
	recs := h.flushSink()
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	r := recs[0]
	if r.SandboxID != "sb" || r.TenantID != "tenant-1" || r.Labels["session_id"] != "s1" || r.Model != "claude-opus-5-5" ||
		r.InputTokens != 10 || r.OutputTokens != 42 || r.CacheReadInputTokens != 500 || r.CacheCreationInputTokens != 20 ||
		!r.Complete || !r.Streamed || r.ProviderRequestID != "req_123" || r.Path != "/v1/messages" || r.Status != 200 {
		t.Fatalf("record = %+v", r)
	}

	for _, p := range []string{"/v1/messages/batches", "/v1/complete", "/v1//messages", "/v1/%6dessages", "/v1/messages/"} {
		resp, err := h.guest.Post("https://"+host+p, "application/json", strings.NewReader(`{}`))
		expectRefusal(t, resp, err, 403, ReasonPathNotAllowed)
	}

	h.srv.SetModelBlocked("sb", true)
	resp, err = h.guest.Post("https://"+host+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get(RefusalHeader) != ReasonModelBlocked || !strings.Contains(string(body), `"type":"error"`) {
		t.Fatalf("blocked: %d %s", resp.StatusCode, body)
	}
}

// Without a policy the proxy serves nothing.
func TestUnknownSandboxRefused(t *testing.T) {
	ca, _ := NewCA()
	srv := New(Config{CA: ca, Resolver: fakeResolver{}})
	ts := httptest.NewServer(srv.Handler(func(string) (string, bool) { return "", false }))
	defer ts.Close()
	proxyURL, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://anything.test/")
	expectRefusal(t, resp, err, 403, ReasonHostNotAllowed)
}
