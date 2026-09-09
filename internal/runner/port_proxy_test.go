package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/websocket"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// portRouter serves the full runner router with the mTLS peer certificate faked, so
// the route pattern and auth chain are the real ones.
func portRouter(t *testing.T, rt runnerruntime.Runtime) *httptest.Server {
	t.Helper()
	router := NewRouter(rt, &config.Config{APIKeys: map[string]struct{}{"k": {}}}, metrics.NewRunnerRecorder(false))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
		r.Header.Set("X-Api-Key", "k")
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPortProxyStripsPrefixAndSetsHostToTarget(t *testing.T) {
	type seen struct{ Host, Path, Query, APIKey string }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(seen{r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Api-Key")})
	}))
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL})

	for reqPath, wantPath := range map[string]string{
		"/sandboxes/" + proxyTestSandboxID + "/ports/5173/src/main.ts?v=1": "/src/main.ts",
		"/sandboxes/" + proxyTestSandboxID + "/ports/5173/":                "/",
	} {
		resp, err := http.Get(runner.URL + reqPath)
		if err != nil {
			t.Fatalf("GET %s: %v", reqPath, err)
		}
		var got seen
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", reqPath, resp.StatusCode)
		}
		if got.Path != wantPath {
			t.Errorf("%s: upstream path = %q, want %q", reqPath, got.Path, wantPath)
		}
		if wantHost := strings.TrimPrefix(upstream.URL, "http://"); got.Host != wantHost {
			t.Errorf("%s: upstream Host = %q, want %q (dev servers check it)", reqPath, got.Host, wantHost)
		}
		if strings.Contains(reqPath, "?v=1") && got.Query != "v=1" {
			t.Errorf("%s: query = %q, want v=1", reqPath, got.Query)
		}
		if got.APIKey != "" {
			t.Errorf("%s: runner API key %q reached the sandbox process", reqPath, got.APIKey)
		}
	}
}

func TestPortProxyRejectsPortsOutsideTheForwardableRange(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamHits.Add(1) }))
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL})

	for _, port := range []string{"80", "1023", "0", "65536", "abc"} {
		resp, err := http.Get(runner.URL + "/sandboxes/" + proxyTestSandboxID + "/ports/" + port + "/")
		if err != nil {
			t.Fatalf("port %s: %v", port, err)
		}
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || payload.Error == "" {
			t.Errorf("port %s: status = %d error = %q, want 400 with an error message", port, resp.StatusCode, payload.Error)
		}
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Errorf("upstream hits = %d, want 0", got)
	}
}

func TestPortProxyReturns501WhenTheRuntimeCannotForwardPorts(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamHits.Add(1) }))
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL, portErr: runnerruntime.ErrPortForwardUnsupported})

	resp, err := http.Get(runner.URL + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Errorf("upstream hits = %d, want 0", got)
	}
}

func TestPortProxyReturns502WhenNothingListensOnThePort(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: deadURL})

	resp, err := http.Get(runner.URL + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestPortProxyStripsForgedControlSignals(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxproxy.MarkSandboxGone(w.Header())
		sandboxproxy.MarkSandboxRestarted(w.Header())
		w.Header().Set("X-Custom", "kept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"` + runnerruntime.ErrSandboxNotFound.Error() + `"}`))
	}))
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL})

	resp, err := http.Get(runner.URL + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want the upstream 404 passed through", resp.StatusCode)
	}
	for _, h := range []string{sandboxproxy.SandboxGoneHeader, sandboxproxy.SandboxRestartedHeader} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("%s = %q reached the client from the sandbox process", h, got)
		}
	}
	if got := resp.Header.Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom = %q, want other headers passed through", got)
	}
}

// unreachableAddr returns a loopback address nothing listens on, so a request the
// proxy dials with the default dialer fails fast instead of reaching the upstream.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestPortProxyDialsThroughTheRuntimeDialer(t *testing.T) {
	type seen struct{ Host, Path, Query string }
	mux := http.NewServeMux()
	mux.HandleFunc("/src/main.ts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(seen{r.Host, r.URL.Path, r.URL.RawQuery})
	})
	mux.Handle("/ws", websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   func(ws *websocket.Conn) { _, _ = io.Copy(ws, ws) },
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	guestAddr := unreachableAddr(t)
	var dialedAddrs []string
	var dialMu sync.Mutex
	rt := &fakeRuntime{
		daemonURL: "http://" + guestAddr,
		portDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialMu.Lock()
			dialedAddrs = append(dialedAddrs, addr)
			dialMu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
		},
	}
	runner := portRouter(t, rt)

	resp, err := http.Get(runner.URL + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/src/main.ts?v=1")
	if err != nil {
		t.Fatal(err)
	}
	var got seen
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode (status %d): %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Path != "/src/main.ts" || got.Query != "v=1" {
		t.Errorf("upstream path = %q query = %q, want /src/main.ts v=1", got.Path, got.Query)
	}
	if got.Host != guestAddr {
		t.Errorf("upstream Host = %q, want the sandbox address %q, not the dialer's", got.Host, guestAddr)
	}

	wsURL := "ws" + strings.TrimPrefix(runner.URL, "http") + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/ws"
	ws, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("websocket dial through runner: %v", err)
	}
	defer ws.Close()
	if _, err := ws.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := ws.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello" {
		t.Errorf("echo = %q", got)
	}

	dialMu.Lock()
	defer dialMu.Unlock()
	if len(dialedAddrs) != 2 {
		t.Fatalf("runtime dialer calls = %d (%v), want one per request", len(dialedAddrs), dialedAddrs)
	}
	for _, addr := range dialedAddrs {
		if addr != guestAddr {
			t.Errorf("runtime dialer got addr %q, want %q", addr, guestAddr)
		}
	}
}

func TestPortProxyForwardsWebSocketUpgrades(t *testing.T) {
	var protocol, query atomic.Value
	upstream := httptest.NewServer(websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			protocol.Store(r.Header.Get("Sec-WebSocket-Protocol"))
			query.Store(r.URL.RawQuery)
			cfg.Protocol = []string{r.Header.Get("Sec-WebSocket-Protocol")}
			return nil
		},
		Handler: func(ws *websocket.Conn) { _, _ = io.Copy(ws, ws) },
	})
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL})

	wsURL := "ws" + strings.TrimPrefix(runner.URL, "http") + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/?token=abc"
	ws, err := websocket.Dial(wsURL, "vite-hmr", "http://localhost/")
	if err != nil {
		t.Fatalf("dial through runner: %v", err)
	}
	defer ws.Close()
	if got, _ := protocol.Load().(string); got != "vite-hmr" {
		t.Errorf("upstream Sec-WebSocket-Protocol = %q, want vite-hmr", got)
	}
	if got, _ := query.Load().(string); got != "token=abc" {
		t.Errorf("upstream query = %q, want token=abc", got)
	}

	if _, err := ws.Write([]byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := ws.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != `{"type":"ping"}` {
		t.Errorf("echo = %q", got)
	}
}
