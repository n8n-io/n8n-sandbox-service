package runner

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/websocket"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
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
	type seen struct{ Host, Path, Query string }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(seen{r.Host, r.URL.Path, r.URL.RawQuery})
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

func TestPortProxyForwardsWebSocketUpgrades(t *testing.T) {
	upstream := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		_, _ = io.Copy(ws, ws)
	}))
	defer upstream.Close()
	runner := portRouter(t, &fakeRuntime{daemonURL: upstream.URL})

	wsURL := "ws" + strings.TrimPrefix(runner.URL, "http") + "/sandboxes/" + proxyTestSandboxID + "/ports/5173/?token=abc"
	ws, err := websocket.Dial(wsURL, "vite-hmr", "http://localhost/")
	if err != nil {
		t.Fatalf("dial through runner: %v", err)
	}
	defer ws.Close()

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
