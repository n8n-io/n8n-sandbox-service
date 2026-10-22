package grpctls

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestKeyPairReloaderReloadsOnMtime(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeTestPair(t, certPath, keyPath, "first")

	r := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := r.Prime(); err != nil {
		t.Fatal(err)
	}
	c1, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Certificate) == 0 {
		t.Fatal("empty cert")
	}

	time.Sleep(20 * time.Millisecond)
	writeTestPair(t, certPath, keyPath, "second")
	c2, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.Certificate[0]) == string(c2.Certificate[0]) {
		t.Fatal("expected certificate bytes to change after file update")
	}
}

// A refresh that has replaced the cert but not yet the key, or files that are
// broken, must not take down a listener that already has a working pair.
func TestKeyPairReloaderKeepsPreviousPairWhenReloadFails(t *testing.T) {
	setRetryInterval(t, 0)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeTestPair(t, certPath, keyPath, "first")

	r := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := r.Prime(); err != nil {
		t.Fatal(err)
	}
	first := mustCurrentCN(t, r)

	// New cert, old key.
	second := newTestPair(t, "second")
	time.Sleep(20 * time.Millisecond)
	writeFile(t, certPath, second.certPEM)
	if got := mustCurrentCN(t, r); got != first {
		t.Fatalf("mismatched pair: serving %q, want the previous %q", got, first)
	}

	// Empty cert.
	time.Sleep(20 * time.Millisecond)
	writeFile(t, certPath, nil)
	if got := mustCurrentCN(t, r); got != first {
		t.Fatalf("empty cert: serving %q, want the previous %q", got, first)
	}

	// Missing key.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if got := mustCurrentCN(t, r); got != first {
		t.Fatalf("missing key: serving %q, want the previous %q", got, first)
	}

	// The matching key lands: the new pair takes over.
	time.Sleep(20 * time.Millisecond)
	writeFile(t, certPath, second.certPEM)
	writeFile(t, keyPath, second.keyPEM)
	if got := mustCurrentCN(t, r); got != "second" {
		t.Fatalf("after the key landed: serving %q, want second", got)
	}
}

// Files rewritten within the same mtime tick as a failed load must still be
// picked up: the reloader retries an unchanged stamp after reloadRetryInterval.
func TestKeyPairReloaderRetriesFailedFilesWithUnchangedMtime(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeTestPair(t, certPath, keyPath, "first")

	r := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := r.Prime(); err != nil {
		t.Fatal(err)
	}

	stamp := time.Now().Add(time.Minute).Truncate(time.Second)
	second := newTestPair(t, "second")
	writeFile(t, certPath, second.certPEM)
	setMtime(t, stamp, certPath, keyPath)

	setRetryInterval(t, time.Hour)
	if got := mustCurrentCN(t, r); got != "first" {
		t.Fatalf("mismatched pair: serving %q, want first", got)
	}

	writeFile(t, keyPath, second.keyPEM)
	setMtime(t, stamp, certPath, keyPath)
	if got := mustCurrentCN(t, r); got != "first" {
		t.Fatalf("within the retry interval: serving %q, want first", got)
	}

	setRetryInterval(t, 0)
	if got := mustCurrentCN(t, r); got != "second" {
		t.Fatalf("after the retry interval: serving %q, want second", got)
	}
}

func TestKeyPairReloaderPrimeFailsOnMismatchedPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeFile(t, certPath, newTestPair(t, "a").certPEM)
	writeFile(t, keyPath, newTestPair(t, "b").keyPEM)

	r := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := r.Prime(); err == nil {
		t.Fatal("Prime succeeded on a cert and key that do not match")
	}
	if _, err := r.GetCertificate(nil); err == nil {
		t.Fatal("GetCertificate succeeded without a pair ever loading")
	}
}

// Credentials built per call share the listener's reloader, so files caught
// mid-renewal don't fail a call either.
func TestConfigsForTheSameFilesShareOneReloader(t *testing.T) {
	setRetryInterval(t, 0)
	dir := t.TempDir()
	ca := newTestCA(t)
	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, ca.certPEM)
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	ca.issue(t, "client-1", x509.ExtKeyUsageClientAuth, certPath, keyPath)

	if _, err := NewClientTLSConfig(caPath, certPath, keyPath, "server"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	writeFile(t, certPath, newTestPair(t, "other").certPEM)

	conf, err := NewClientTLSConfig(caPath, certPath, keyPath, "server")
	if err != nil {
		t.Fatalf("second config for files caught mid-renewal: %v", err)
	}
	c, err := conf.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Leaf.Subject.CommonName; got != "client-1" {
		t.Fatalf("second config presents %q, want the previous client-1", got)
	}
}

func TestNotePeerUnaryClientInterceptorNotesTheServerCertificate(t *testing.T) {
	leaf := newTestPair(t, "runner-control").leaf
	invoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, opts ...grpc.CallOption) error {
		for _, o := range opts {
			if po, ok := o.(grpc.PeerCallOption); ok {
				*po.PeerAddr = peer.Peer{AuthInfo: credentials.TLSInfo{
					State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}},
				}}
			}
		}
		return nil
	}
	if err := NotePeerUnaryClientInterceptor(context.Background(), "/m", nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	if NotePeer(leaf) {
		t.Fatal("the interceptor did not note the server certificate")
	}
}

func TestCertificateExpiryPicksUpRenewedFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	first := newTestPairValidFor(t, "first", 24*time.Hour)
	writeFile(t, certPath, first.certPEM)
	writeFile(t, keyPath, first.keyPEM)

	r := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := r.Prime(); err != nil {
		t.Fatal(err)
	}
	if got := CertificateExpiry()[certPath]; !got.Equal(first.notAfter) {
		t.Fatalf("expiry = %v, want %v", got, first.notAfter)
	}

	// No handshake in between: the expiry still follows the files.
	second := newTestPairValidFor(t, "second", 48*time.Hour)
	time.Sleep(20 * time.Millisecond)
	writeFile(t, certPath, second.certPEM)
	writeFile(t, keyPath, second.keyPEM)
	if got := CertificateExpiry()[certPath]; !got.Equal(second.notAfter) {
		t.Fatalf("expiry after renewal = %v, want %v", got, second.notAfter)
	}
}

func TestNoteLoadedLogsEachCertificateOncePerPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cert.pem")
	a := newTestPair(t, "a").leaf
	b := newTestPair(t, "b").leaf
	for i, tc := range []struct {
		leaf *x509.Certificate
		want bool
	}{{a, true}, {a, false}, {b, true}, {b, false}, {a, true}} {
		if got := noteLoaded(path, tc.leaf); got != tc.want {
			t.Errorf("call %d: noteLoaded = %v, want %v", i, got, tc.want)
		}
	}
}

func TestNotePeerLogsEachCertificateOnce(t *testing.T) {
	a := newTestPair(t, "a").leaf
	b := newTestPair(t, "b").leaf
	for i, tc := range []struct {
		leaf *x509.Certificate
		want bool
	}{{a, true}, {a, false}, {b, true}, {a, false}, {b, false}} {
		if got := NotePeer(tc.leaf); got != tc.want {
			t.Errorf("call %d: NotePeer = %v, want %v", i, got, tc.want)
		}
	}
}

// A renewed certificate for a subject seen before is logged again.
func TestNotePeerLogsRenewalsOfTheSameSubject(t *testing.T) {
	first := newTestPair(t, "runner-control").leaf
	renewed := newTestPair(t, "runner-control").leaf
	if !NotePeer(first) || !NotePeer(renewed) {
		t.Fatal("first and renewed certificate must both be logged")
	}
	if NotePeer(renewed) {
		t.Fatal("the renewed certificate was logged twice")
	}
}

func TestPeerCertificate(t *testing.T) {
	leaf := newTestPair(t, "runner").leaf
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}},
	})
	if got := PeerCertificate(ctx); got != leaf {
		t.Fatalf("PeerCertificate = %v, want the handshake's leaf", got)
	}
	if got := PeerCertificate(context.Background()); got != nil {
		t.Fatalf("PeerCertificate without a peer = %v, want nil", got)
	}
	noTLS := peer.NewContext(context.Background(), &peer.Peer{})
	if got := PeerCertificate(noTLS); got != nil {
		t.Fatalf("PeerCertificate without TLS = %v, want nil", got)
	}
}

func TestCertSerialMatchesOpenSSL(t *testing.T) {
	c := &x509.Certificate{SerialNumber: new(big.Int).SetBytes([]byte{0x08, 0x53, 0xfb, 0x42})}
	if got := CertSerial(c); got != "0853FB42" {
		t.Fatalf("CertSerial = %q, want 0853FB42", got)
	}
}

type testPair struct {
	certPEM, keyPEM []byte
	leaf            *x509.Certificate
	notAfter        time.Time
}

func newTestPair(t *testing.T, cn string) testPair {
	return newTestPairValidFor(t, cn, 24*time.Hour)
}

func newTestPairValidFor(t *testing.T, cn string, validFor time.Duration) testPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(validFor).Truncate(time.Second)
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return testPair{
		certPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		leaf:     leaf,
		notAfter: notAfter,
	}
}

func writeTestPair(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	p := newTestPair(t, cn)
	writeFile(t, certPath, p.certPEM)
	writeFile(t, keyPath, p.keyPEM)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, mtime time.Time, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func setRetryInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := reloadRetryInterval
	reloadRetryInterval = d
	t.Cleanup(func() { reloadRetryInterval = prev })
}

func mustCurrentCN(t *testing.T, r *KeyPairReloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	return c.Leaf.Subject.CommonName
}
