package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// testCA signs the certificates the API and a runner present to each other.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string
}

func newTestCA(t *testing.T, dir, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := testCA{cert: cert, key: key, file: filepath.Join(dir, name+".crt")}
	writeTestPEM(t, ca.file, "CERTIFICATE", der)
	return ca
}

// issue writes a leaf certificate and its key as <dir>/<name>.crt and .key.
func (ca testCA) issue(t *testing.T, dir, name string, usage x509.ExtKeyUsage) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	writeTestPEM(t, certFile, "CERTIFICATE", der)
	writeTestPEM(t, keyFile, "PRIVATE KEY", keyDER)
	return certFile, keyFile
}

func writeTestPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// volumeObjectsEnv is a volume-objects listener started the way main starts
// it, with the certificates a runner would use to reach it.
type volumeObjectsEnv struct {
	addr      string
	store     *volumestore.Local
	serverCAs *x509.CertPool
	// runnerCert is signed by the CA the listener trusts, strangerCert by
	// another one.
	runnerCert   *tls.Certificate
	strangerCert *tls.Certificate
}

func startVolumeObjectsServer(t *testing.T) volumeObjectsEnv {
	t.Helper()
	dir := t.TempDir()
	registrationCA := newTestCA(t, dir, "registration-ca")
	otherCA := newTestCA(t, dir, "other-ca")
	serverCert, serverKey := registrationCA.issue(t, dir, "api", x509.ExtKeyUsageServerAuth)

	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.APIConfig{
		VolumeStore:           config.VolumeStoreLocal,
		VolumeLocalListenAddr: "127.0.0.1:0",
		GRPCServerCertFile:    serverCert,
		GRPCServerKeyFile:     serverKey,
		GRPCClientCAFile:      registrationCA.file,
		MaxInflightPerTenant:  4,
	}
	srv, lis, err := newVolumeObjectsServer(cfg, store, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("newVolumeObjectsServer: %v", err)
	}
	if srv == nil || lis == nil {
		t.Fatal("expected a volume objects server for the local store")
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	serverCAs := x509.NewCertPool()
	serverCAs.AddCert(registrationCA.cert)
	return volumeObjectsEnv{
		addr:         lis.Addr().String(),
		store:        store,
		serverCAs:    serverCAs,
		runnerCert:   loadTestKeyPair(t, registrationCA, dir, "runner"),
		strangerCert: loadTestKeyPair(t, otherCA, dir, "stranger"),
	}
}

func loadTestKeyPair(t *testing.T, ca testCA, dir, name string) *tls.Certificate {
	t.Helper()
	certFile, keyFile := ca.issue(t, dir, name, x509.ExtKeyUsageClientAuth)
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return &pair
}

// clientTLS trusts the listener's CA and presents cert, or no certificate
// when cert is nil. It presents cert whatever CAs the listener asks for: a
// certificate in tls.Config.Certificates is left out when its issuer is not
// among them, which would turn a foreign certificate into no certificate.
func (env volumeObjectsEnv) clientTLS(cert *tls.Certificate) *tls.Config {
	conf := &tls.Config{RootCAs: env.serverCAs, MinVersion: tls.VersionTLS12}
	if cert != nil {
		conf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert, nil
		}
	}
	return conf
}

// refusal returns the TLS alert the listener ended a connection attempt
// with, or "" if the connection stands. In TLS 1.3 the client's handshake
// completes before the server has checked its certificate, so the alert
// arrives with the first read; nothing is written first, since a write could
// meet the closed connection before the alert is read.
func (env volumeObjectsEnv) refusal(t *testing.T, conf *tls.Config) string {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", env.addr, conf)
	if err == nil {
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(make([]byte, 1))
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" {
		return opErr.Err.Error()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ""
	}
	t.Fatalf("connection ended with %v, want a TLS alert from the listener or an open connection", err)
	return ""
}

// signedPath returns the path and query of a read URL for key.
func (env volumeObjectsEnv) signedPath(t *testing.T, key volumestore.Key) string {
	t.Helper()
	signed, err := env.store.ReadURL(context.Background(), key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.RequestURI()
}

func TestNewVolumeObjectsServerSkipsOtherStores(t *testing.T) {
	remote, err := volumestore.NewRemote("/nonexistent.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	for name, volumes := range map[string]volumestore.Store{"no store": nil, "remote": remote} {
		srv, lis, err := newVolumeObjectsServer(&config.APIConfig{}, volumes, metrics.NewAPIRecorder(false))
		if err != nil {
			t.Fatalf("%s: newVolumeObjectsServer: %v", name, err)
		}
		if srv != nil || lis != nil {
			t.Fatalf("%s: expected no volume objects server, got srv=%v lis=%v", name, srv, lis)
		}
	}
}

func TestVolumeObjectsServerRefusesConnectionsWithoutARunnerCertificate(t *testing.T) {
	env := startVolumeObjectsServer(t)

	tls12 := env.clientTLS(env.runnerCert)
	tls12.MaxVersion = tls.VersionTLS12
	// Each case must fail for its own reason, so the alerts are compared too.
	for _, testCase := range []struct {
		name      string
		conf      *tls.Config
		wantAlert string
	}{
		{"no client certificate", env.clientTLS(nil), "certificate required"},
		{"a certificate from another CA", env.clientTLS(env.strangerCert), "unknown certificate authority"},
		{"TLS 1.2", tls12, "protocol version not supported"},
		{"a runner certificate", env.clientTLS(env.runnerCert), ""},
	} {
		got := env.refusal(t, testCase.conf)
		if testCase.wantAlert == "" && got != "" {
			t.Errorf("%s: refused with %q, want the connection accepted", testCase.name, got)
		}
		if testCase.wantAlert != "" && !strings.Contains(got, testCase.wantAlert) {
			t.Errorf("%s: alert %q, want %q", testCase.name, got, testCase.wantAlert)
		}
	}
}

func TestVolumeObjectsServerServesSignedURLsToRunners(t *testing.T) {
	env := startVolumeObjectsServer(t)
	key := volumestore.Key{Scope: volume.SharedScope, ID: "python", Kind: volume.KindExt4}
	if _, err := env.store.Put(context.Background(), key, strings.NewReader("image bytes"), volumestore.PutOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}
	signed := env.signedPath(t, key)
	unsigned, _, _ := strings.Cut(signed, "?")

	transport := &http.Transport{TLSClientConfig: env.clientTLS(env.runnerCert)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	get := func(target string) (int, string) {
		t.Helper()
		resp, err := client.Get("https://" + env.addr + target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer resp.Body.Close()
		if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
			t.Errorf("GET %s: not served over TLS 1.3", target)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: read body: %v", target, err)
		}
		return resp.StatusCode, string(body)
	}

	if status, body := get(signed); status != http.StatusOK || body != "image bytes" {
		t.Errorf("signed URL: %d %q, want 200 \"image bytes\"", status, body)
	}
	// The certificate opens the connection; only the signature opens an object.
	if status, _ := get(unsigned); status != http.StatusForbidden {
		t.Errorf("no signature: %d, want 403", status)
	}
	if status, _ := get(strings.Replace(signed, "python", "node", 1)); status != http.StatusForbidden {
		t.Errorf("signature of another object: %d, want 403", status)
	}
	for _, other := range []string{"/", "/healthz", "/metrics", "/sandboxes", "/admin/tenants"} {
		if status, _ := get(other); status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404: the listener serves volume objects only", other, status)
		}
	}
}

func TestNewVolumeObjectsServerFailsOnOccupiedPort(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer squatter.Close()

	dir := t.TempDir()
	registrationCA := newTestCA(t, dir, "registration-ca")
	serverCert, serverKey := registrationCA.issue(t, dir, "api", x509.ExtKeyUsageServerAuth)
	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.APIConfig{
		VolumeLocalListenAddr: squatter.Addr().String(),
		GRPCServerCertFile:    serverCert,
		GRPCServerKeyFile:     serverKey,
		GRPCClientCAFile:      registrationCA.file,
	}

	srv, lis, err := newVolumeObjectsServer(cfg, store, metrics.NewAPIRecorder(false))
	if err == nil {
		if lis != nil {
			lis.Close()
		}
		t.Fatal("expected a bind error for an occupied volume objects port")
	}
	if srv != nil || lis != nil {
		t.Fatalf("expected nothing back on error, got srv=%v lis=%v", srv, lis)
	}
}

func TestNewVolumeObjectsServerNeedsTheRegistrationCertificates(t *testing.T) {
	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.pem")
	cfg := &config.APIConfig{
		VolumeLocalListenAddr: "127.0.0.1:0",
		GRPCServerCertFile:    missing,
		GRPCServerKeyFile:     missing,
		GRPCClientCAFile:      missing,
	}
	srv, lis, err := newVolumeObjectsServer(cfg, store, metrics.NewAPIRecorder(false))
	if err == nil {
		if lis != nil {
			lis.Close()
		}
		t.Fatal("expected an error without the registration certificates")
	}
	if srv != nil || lis != nil {
		t.Fatalf("expected nothing back on error, got srv=%v lis=%v", srv, lis)
	}
}
