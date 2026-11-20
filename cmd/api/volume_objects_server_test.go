package main

import (
	"bytes"
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

	"github.com/n8n-io/sandbox-service/internal/api"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
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

// startVolumeObjectsServer allows maxInflight downloads per scope, each until
// its client has read nothing for stallTimeout.
func startVolumeObjectsServer(t *testing.T, maxInflight int, stallTimeout time.Duration) volumeObjectsEnv {
	t.Helper()
	dir := t.TempDir()
	registrationCA := newTestCA(t, dir, "registration-ca")
	otherCA := newTestCA(t, dir, "other-ca")
	serverCert, serverKey := registrationCA.issue(t, dir, "api", x509.ExtKeyUsageServerAuth)

	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443", volumestore.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.APIConfig{
		VolumeStore:           config.VolumeStoreLocal,
		VolumeLocalListenAddr: "127.0.0.1:0",
		GRPCServerCertFile:    serverCert,
		GRPCServerKeyFile:     serverKey,
		GRPCClientCAFile:      registrationCA.file,
		MaxInflightPerTenant:  maxInflight,
	}
	srv, lis, err := newVolumeObjectsServer(cfg, store, stallTimeout, metrics.NewAPIRecorder(false))
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
		srv, lis, err := newVolumeObjectsServer(&config.APIConfig{}, volumes, api.VolumeObjectStallTimeout, metrics.NewAPIRecorder(false))
		if err != nil {
			t.Fatalf("%s: newVolumeObjectsServer: %v", name, err)
		}
		if srv != nil || lis != nil {
			t.Fatalf("%s: expected no volume objects server, got srv=%v lis=%v", name, srv, lis)
		}
	}
}

func TestVolumeObjectsServerRefusesConnectionsWithoutARunnerCertificate(t *testing.T) {
	env := startVolumeObjectsServer(t, 4, api.VolumeObjectStallTimeout)

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
	env := startVolumeObjectsServer(t, 4, api.VolumeObjectStallTimeout)
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
	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443", volumestore.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.APIConfig{
		VolumeLocalListenAddr: squatter.Addr().String(),
		GRPCServerCertFile:    serverCert,
		GRPCServerKeyFile:     serverKey,
		GRPCClientCAFile:      registrationCA.file,
	}

	srv, lis, err := newVolumeObjectsServer(cfg, store, api.VolumeObjectStallTimeout, metrics.NewAPIRecorder(false))
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
	store, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443", volumestore.LocalOptions{})
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
	srv, lis, err := newVolumeObjectsServer(cfg, store, api.VolumeObjectStallTimeout, metrics.NewAPIRecorder(false))
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

// A runner that stops reading its socket, as a paused or frozen one does,
// blocks the whole HTTP/2 connection. The stall timeout then only queues a
// stream reset behind data that never leaves, so the connection's own write
// timeout has to close the connection and give the slot back. Go's client
// keeps reading its socket whatever the caller does, so this client writes
// HTTP/2 frames itself.
func TestVolumeObjectsServerFreesTheSlotOfAStalledHTTP2Connection(t *testing.T) {
	const stallTimeout = 300 * time.Millisecond
	env := startVolumeObjectsServer(t, 1, stallTimeout)
	key := volumestore.Key{Scope: volume.SharedScope, ID: "large", Kind: volume.KindExt4}
	const size = 32 << 20 // more than the socket buffers on both ends hold
	if _, err := env.store.Put(context.Background(), key, bytes.NewReader(make([]byte, size)), volumestore.PutOptions{MaxBytes: size}); err != nil {
		t.Fatal(err)
	}
	signed := env.signedPath(t, key)

	conf := env.clientTLS(env.runnerCert)
	conf.NextProtos = []string{"h2"}
	stalled, err := tls.Dial("tcp", env.addr, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if protocol := stalled.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		t.Fatalf("negotiated %q, want h2", protocol)
	}
	if _, err := io.WriteString(stalled, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(stalled, stalled)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	// The largest windows, so that only the socket holds the server back.
	const maxWindow = 1<<31 - 1
	if err := framer.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: maxWindow}); err != nil {
		t.Fatal(err)
	}
	if err := framer.WriteWindowUpdate(0, maxWindow-65535); err != nil {
		t.Fatal(err)
	}
	var headerBlock bytes.Buffer
	encoder := hpack.NewEncoder(&headerBlock)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: http.MethodGet},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: env.addr},
		{Name: ":path", Value: signed},
	} {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: headerBlock.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	// Once the response has started the download holds the slot; then stop
	// reading altogether.
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	for responseStarted := false; !responseStarted; {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("read the response: %v", err)
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.MetaHeadersFrame:
			if status := frame.PseudoValue("status"); status != "200" {
				t.Fatalf("stalled download status %q, want 200", status)
			}
			responseStarted = true
		case *http2.RSTStreamFrame, *http2.GoAwayFrame:
			t.Fatalf("the server ended the download early: %v", frame)
		}
	}

	transport := &http.Transport{TLSClientConfig: env.clientTLS(env.runnerCert)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	sawLimit := false
	// Closing the connection takes up to 5 s more: crypto/tls gives its close
	// alert that long to get into the full socket.
	deadline := time.Now().Add(stallTimeout + 10*time.Second)
	for {
		resp, err := client.Get("https://" + env.addr + signed)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("got %d, want 429 or 200", resp.StatusCode)
		}
		sawLimit = true
		if time.Now().After(deadline) {
			t.Fatal("the stalled HTTP/2 download still holds its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawLimit {
		t.Fatal("the stalled download never held the slot, so the test proves nothing")
	}
	// On macOS a blocked connection moves again after about 5 s, which lets
	// the reset out and frees the slot without the write timeout, but leaves
	// the connection open. Only the write timeout closes it.
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.Copy(io.Discard, stalled)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("the stalled connection is still open, so its write timeout did not free the slot")
	}
}
