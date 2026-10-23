package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
)

// testPKI is a throwaway CA plus the leaf certificates the runner and the API
// present to each other, written to disk the way the real deployment mounts them.
type testPKI struct {
	caFile         string
	serverCertFile string
	serverKeyFile  string
	clientCertFile string
	clientKeyFile  string
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	pki := testPKI{
		caFile:         filepath.Join(dir, "ca.crt"),
		serverCertFile: filepath.Join(dir, "server.crt"),
		serverKeyFile:  filepath.Join(dir, "server.key"),
		clientCertFile: filepath.Join(dir, "client.crt"),
		clientKeyFile:  filepath.Join(dir, "client.key"),
	}
	writePEM(t, pki.caFile, "CERTIFICATE", caDER)

	writeLeaf(t, caCert, caKey, 2, x509.ExtKeyUsageServerAuth, pki.serverCertFile, pki.serverKeyFile)
	writeLeaf(t, caCert, caKey, 3, x509.ExtKeyUsageClientAuth, pki.clientCertFile, pki.clientKeyFile)
	return pki
}

func writeLeaf(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey, serial int64, usage x509.ExtKeyUsage, certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, certPath, "CERTIFICATE", der)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveMTLS starts the auth-wrapped handler over TLS the way app.Main does, and
// returns its base URL.
func serveMTLS(t *testing.T, pki testPKI) string {
	t.Helper()
	tlsConf, err := grpctls.NewServerTLSConfig(pki.serverCertFile, pki.serverKeyFile, pki.caFile, tls.VerifyClientCertIfGiven)
	if err != nil {
		t.Fatal(err)
	}

	handler := AuthMiddleware(map[string]struct{}{"secret": {}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, TLSConfig: tlsConf}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	return "https://" + ln.Addr().String()
}

func caPool(t *testing.T, caFile string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("no certificates in test CA")
	}
	return pool
}

func TestRunnerHTTPListenerAcceptsClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	baseURL := serveMTLS(t, pki)

	clientTLS, err := grpctls.NewClientTLSConfig(pki.caFile, pki.clientCertFile, pki.clientKeyFile, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/sandboxes/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request with client certificate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, resp.StatusCode)
	}
}

func TestRunnerHTTPListenerRejectsCallerWithoutClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	baseURL := serveMTLS(t, pki)

	// Trusts the runner and holds a valid API key, but presents no certificate:
	// the handshake succeeds under VerifyClientCertIfGiven and the middleware refuses.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    caPool(t, pki.caFile),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}}}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/sandboxes/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request without client certificate should reach the handler: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, resp.StatusCode)
	}
}

func TestRunnerHTTPListenerAllowsHealthWithoutClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	baseURL := serveMTLS(t, pki)

	// Kubernetes httpGet probes cannot present a client certificate, so health
	// must stay reachable without one.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    caPool(t, pki.caFile),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}}}

	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("probe request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, resp.StatusCode)
	}
}

func TestRunnerHTTPListenerRejectsUntrustedClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	baseURL := serveMTLS(t, pki)

	// A certificate from a different CA is refused during the handshake, not by
	// the middleware, so the request never reaches the handler at all.
	other := newTestPKI(t)
	clientTLS, err := grpctls.NewClientTLSConfig(pki.caFile, other.clientCertFile, other.clientKeyFile, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/sandboxes/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "secret")

	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected handshake failure, got status %d", resp.StatusCode)
	}
}

// serveControlGRPC starts SandboxControl with the listener credentials app.Run
// uses, and returns its address.
func serveControlGRPC(t *testing.T, pki testPKI, cd *cordon.State) string {
	t.Helper()
	creds, err := grpctls.NewServerTransportCredentials(pki.serverCertFile, pki.serverKeyFile, pki.caFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterSandboxControlServer(srv, &SandboxControlGRPC{
		Runtime: &fakeRuntime{},
		Cfg:     &config.Config{RunnerID: "runner-a", APIKeys: map[string]struct{}{"secret": {}}},
		Rec:     metrics.NewRunnerRecorder(false),
		Cordon:  cd,
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func TestCordonOverControlGRPC(t *testing.T) {
	pki := newTestPKI(t)
	cd := cordon.New()
	addr := serveControlGRPC(t, pki, cd)
	ctx := context.Background()
	tlsCfg := &runnerctl.TLS{CAFile: pki.caFile, CertFile: pki.clientCertFile, KeyFile: pki.clientKeyFile, ServerName: "localhost"}

	if got, err := runnerctl.SetCordon(ctx, addr, "secret", tlsCfg, "runner-a", true); err != nil || !got {
		t.Fatalf("SetCordon(true) = %v, %v; want true", got, err)
	}
	if got, err := runnerctl.GetCordon(ctx, addr, "secret", tlsCfg); err != nil || !got {
		t.Fatalf("GetCordon() = %v, %v; want true", got, err)
	}
	if got, err := runnerctl.SetCordon(ctx, addr, "secret", tlsCfg, "runner-a", false); err != nil || got {
		t.Fatalf("SetCordon(false) = %v, %v; want false", got, err)
	}

	if _, err := runnerctl.SetCordon(ctx, addr, "wrong", tlsCfg, "runner-a", true); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("SetCordon with a wrong key: error = %v, want Unauthenticated", err)
	}

	// No client certificate: the handshake fails before the key is looked at.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    caPool(t, pki.caFile),
		ServerName: "localhost",
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	callCtx := metadata.AppendToOutgoingContext(ctx, "x-api-key", "secret")
	if _, err := pb.NewSandboxControlClient(conn).SetCordon(callCtx, &pb.SetCordonRequest{Cordoned: true}); err == nil {
		t.Fatal("SetCordon without a client certificate succeeded")
	}

	if cd.Cordoned() {
		t.Fatal("a rejected call cordoned the runner")
	}
}
