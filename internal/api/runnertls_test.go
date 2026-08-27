package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
)

// Runner HTTP listeners require mTLS, so any test that proxies needs a
// CA-signed server on the runner side and matching client material in the API
// config. The PKI is immutable, so it is built once for the whole package
// rather than per test.
type runnerPKI struct {
	dir        string
	serverCert tls.Certificate
}

var testRunnerPKI = sync.OnceValue(buildRunnerPKI)

func buildRunnerPKI() *runnerPKI {
	dir, err := os.MkdirTemp("", "api-runner-pki")
	if err != nil {
		panic(err)
	}

	caKey, caCert, caDER := issue(nil, nil, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "api-test-ca"},
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	})
	writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)

	// httptest listeners bind loopback, and the transport verifies against the
	// dial host, so the server certificate needs loopback IP SANs.
	serverKey, _, serverDER := issue(caCert, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-runner"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	})
	serverCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		keyPEM(serverKey),
	)
	if err != nil {
		panic(err)
	}

	clientKey, _, clientDER := issue(caCert, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "test-api-client"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	writePEM(filepath.Join(dir, "client.crt"), "CERTIFICATE", clientDER)
	if err := os.WriteFile(filepath.Join(dir, "client.key"), keyPEM(clientKey), 0o600); err != nil {
		panic(err)
	}

	return &runnerPKI{dir: dir, serverCert: serverCert}
}

// issue signs tpl with parent, or self-signs it when parent is nil.
// It panics rather than returning errors: it only runs during package setup,
// where there is no test to fail.
func issue(parent *x509.Certificate, parentKey *ecdsa.PrivateKey, tpl *x509.Certificate) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	tpl.NotBefore = time.Now().Add(-time.Hour)
	tpl.NotAfter = time.Now().Add(24 * time.Hour)
	if tpl.KeyUsage == 0 {
		tpl.KeyUsage = x509.KeyUsageDigitalSignature
	}
	if parent == nil {
		parent, parentKey = tpl, key
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		panic(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return key, cert, der
}

func keyPEM(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func writePEM(path, blockType string, der []byte) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		panic(err)
	}
}

// withRunnerTLS points the API config at the package PKI so NewGatewayRouter
// builds a proxy transport that trusts newTestRunnerServer.
func withRunnerTLS(cfg *config.APIConfig) *config.APIConfig {
	pki := testRunnerPKI()
	cfg.RunnerControlGRPCClientCAFile = filepath.Join(pki.dir, "ca.crt")
	cfg.RunnerControlGRPCClientCertFile = filepath.Join(pki.dir, "client.crt")
	cfg.RunnerControlGRPCClientKeyFile = filepath.Join(pki.dir, "client.key")
	return cfg
}

// The API reaches every runner through one shared transport, so a fixed
// ServerName on it would be applied to all of them: net/http only derives the
// verification name from the dialled host when tls.Config.ServerName is empty.
// Pinning one name would mean the host in a runner's advertised base URL is
// never checked, letting any runner holding a certificate for the pinned name
// answer for another.
func TestRunnerTransportVerifiesEachRunnerHostname(t *testing.T) {
	dir := t.TempDir()
	caKey, caCert, caDER := issue(nil, nil, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "runner-ca"},
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	})
	writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)

	// Deliberately covers runner-a.test only.
	srvKey, _, srvDER := issue(caCert, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "runner-a"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"runner-a.test"},
	})
	srvCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		keyPEM(srvKey),
	)
	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}

	cliKey, _, cliDER := issue(caCert, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "api"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	writePEM(filepath.Join(dir, "client.crt"), "CERTIFICATE", cliDER)
	if err := os.WriteFile(filepath.Join(dir, "client.key"), keyPEM(cliKey), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	rt, err := newRunnerTransport(&config.APIConfig{
		RunnerControlGRPCClientCAFile:   filepath.Join(dir, "ca.crt"),
		RunnerControlGRPCClientCertFile: filepath.Join(dir, "client.crt"),
		RunnerControlGRPCClientKeyFile:  filepath.Join(dir, "client.key"),
		// Set for the control gRPC channel, whose dial address need not appear
		// in the certificate. It must not leak into the proxy transport.
		RunnerControlGRPCClientServerName: "runner-a.test",
	})
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}

	// Route every host to the one test listener, so the only thing that varies
	// between the two requests below is the hostname being verified.
	tr := rt.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	// The transport is cloned from http.DefaultTransport, which honours
	// HTTP_PROXY; without this the request becomes a CONNECT to whatever proxy
	// the developer's environment happens to set.
	tr.Proxy = nil
	client := &http.Client{Transport: tr}

	resp, err := client.Get("https://runner-a.test:8080/")
	if err != nil {
		t.Fatalf("runner-a.test is in the certificate, want success: %v", err)
	}
	_ = resp.Body.Close()

	if resp, err := client.Get("https://runner-b.test:8080/"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("runner-b.test is absent from the certificate, want a verification failure")
	}
}

// newTestRunnerServer starts a stand-in runner over TLS, the way a real runner
// now serves its HTTP listener.
func newTestRunnerServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{testRunnerPKI().serverCert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
