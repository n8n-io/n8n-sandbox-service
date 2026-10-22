package grpctls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// Both ends of an mTLS connection present a renewed leaf on the next handshake,
// with no new config and no restart.
func TestRenewedLeavesTakeEffectOnNextHandshake(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)
	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, ca.certPEM)
	srvCert, srvKey := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	cliCert, cliKey := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	ca.issue(t, "server-1", x509.ExtKeyUsageServerAuth, srvCert, srvKey)
	ca.issue(t, "client-1", x509.ExtKeyUsageClientAuth, cliCert, cliKey)

	serverConf, err := NewServerTLSConfig(srvCert, srvKey, caPath, tls.RequireAndVerifyClientCert)
	if err != nil {
		t.Fatal(err)
	}
	clientConf, err := NewClientTLSConfig(caPath, cliCert, cliKey, "server")
	if err != nil {
		t.Fatal(err)
	}

	gotServer, gotClient := handshake(t, serverConf, clientConf)
	if gotServer != "server-1" || gotClient != "client-1" {
		t.Fatalf("first handshake: server presented %q, client presented %q", gotServer, gotClient)
	}

	time.Sleep(20 * time.Millisecond)
	ca.issue(t, "server-2", x509.ExtKeyUsageServerAuth, srvCert, srvKey)
	ca.issue(t, "client-2", x509.ExtKeyUsageClientAuth, cliCert, cliKey)

	gotServer, gotClient = handshake(t, serverConf, clientConf)
	if gotServer != "server-2" || gotClient != "client-2" {
		t.Fatalf("after renewal: server presented %q, client presented %q", gotServer, gotClient)
	}
}

// handshake runs one TLS handshake over a pipe and returns the common names
// each side saw the other present.
func handshake(t *testing.T, serverConf, clientConf *tls.Config) (serverCN, clientCN string) {
	t.Helper()
	sc, cc := net.Pipe()
	defer sc.Close()
	defer cc.Close()
	srv := tls.Server(sc, serverConf)
	cli := tls.Client(cc, clientConf)

	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	serverCN = cli.ConnectionState().PeerCertificates[0].Subject.CommonName
	clientCN = srv.ConnectionState().PeerCertificates[0].Subject.CommonName
	return serverCN, clientCN
}

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue writes a new leaf for cn, signed by the CA, to certPath and keyPath.
func (ca testCA) issue(t *testing.T, cn string, usage x509.ExtKeyUsage, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{"server"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
