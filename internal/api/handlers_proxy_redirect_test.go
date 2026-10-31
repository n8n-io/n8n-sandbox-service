package api

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
)

// TestSandboxProxyRefusesRunnerRedirect: a runner answering a proxied route
// with a 3xx must not have that response relayed, or a compromised runner could
// send the client, and the API key it holds, to a host of its choosing.
func TestSandboxProxyRefusesRunnerRedirect(t *testing.T) {
	logs := captureLogs(t)
	var (
		status       atomic.Int32
		upstreamHits atomic.Int64
	)
	// Long enough that logging it uncapped would be visible in the event.
	location := "https://attacker.example/" + strings.Repeat("a", 4096)
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Location", location)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("moved"))
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	for i, code := range []int{301, 302, 303, 307, 308} {
		status.Store(int32(code))
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadGateway {
			t.Errorf("runner %d: expected %d, got %d body=%s", code, http.StatusBadGateway, rr.Code, rr.Body.String())
		}
		if loc := rr.Header().Get("Location"); loc != "" {
			t.Errorf("runner %d: Location relayed: %q", code, loc)
		}
		var body APIError
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != errRunnerRedirect.Error() {
			t.Errorf("runner %d: unexpected body %s (err=%v)", code, rr.Body.String(), err)
		}
		if hits := upstreamHits.Load(); hits != int64(i+1) {
			t.Fatalf("runner %d: expected %d upstream requests, got %d", code, i+1, hits)
		}
	}

	// The refusal must not count as activity, or a retrying client would keep
	// the idle sweeper from ever reclaiming a sandbox on a misbehaving runner.
	rec, err := s.Get(sid)
	if err != nil || rec == nil {
		t.Fatalf("get sandbox: rec=%v err=%v", rec, err)
	}
	if rec.LastActiveAt != 1 {
		t.Fatalf("LastActiveAt = %d, want 1 unchanged", rec.LastActiveAt)
	}

	// The runner chooses the Location, so the log keeps only a bounded prefix.
	logged := 0
	for _, event := range logs() {
		loc, ok := event["runner_redirect_location"].(string)
		if !ok {
			continue
		}
		logged++
		if len(loc) != 200 {
			t.Fatalf("logged runner_redirect_location has %d bytes, want capped at 200", len(loc))
		}
	}
	if logged != int(upstreamHits.Load()) {
		t.Fatalf("expected %d events with runner_redirect_location, got %d", upstreamHits.Load(), logged)
	}
}

// A refused redirect still came from a runner that authenticated with its
// server certificate, so the proxy notes that certificate as for any response.
// The certificate is the test's own: the package PKI's is noted by every proxy
// test, which would make the assertion depend on test order.
func TestRunnerProxyNotesPeerCertificateOfRedirect(t *testing.T) {
	key, _, der := issue(nil, nil, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "redirecting-runner"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	})
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM(key))
	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}
	runner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example/")
		w.WriteHeader(http.StatusFound)
	}))
	runner.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	runner.StartTLS()
	defer runner.Close()

	u, err := url.Parse(runner.URL)
	if err != nil {
		t.Fatalf("parse runner url: %v", err)
	}
	rr := httptest.NewRecorder()
	newRunnerReverseProxy(u, "", runner.Client().Transport, nil).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/files", nil))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected %d, got %d body=%s", http.StatusBadGateway, rr.Code, rr.Body.String())
	}
	if grpctls.NotePeer(runner.Certificate()) {
		t.Fatal("the proxy did not note the certificate of a runner that answered with a redirect")
	}
}
