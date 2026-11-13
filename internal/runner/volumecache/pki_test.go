package volumecache

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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

var certSerial atomic.Int64

func newTestCA(t testing.TB, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(certSerial.Add(1)),
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
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// leaf issues a certificate named commonName for hosts, each a DNS name or an
// IP address, and returns it and its key as PEM.
func (ca *testCA) leaf(t testing.TB, commonName string, usage x509.ExtKeyUsage, hosts ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(certSerial.Add(1)),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

var storage struct {
	once sync.Once
	ca   *testCA
	cert tls.Certificate
}

// storagePKI is the CA of the test servers that stand in for object storage,
// and the certificate they serve on 127.0.0.1.
func storagePKI(t testing.TB) (*testCA, tls.Certificate) {
	t.Helper()
	storage.once.Do(func() {
		storage.ca = newTestCA(t, "storage test CA")
		certPEM, keyPEM := storage.ca.leaf(t, "storage", x509.ExtKeyUsageServerAuth, "127.0.0.1")
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		storage.cert = cert
	})
	return storage.ca, storage.cert
}

// registrationFiles is the material the API and a runner register with, on
// disk as a deployment mounts it.
type registrationFiles struct {
	ca         string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

// writeRegistrationFiles issues the API's server certificate for apiHosts and
// a runner's client certificate from ca.
func writeRegistrationFiles(t testing.TB, ca *testCA, apiHosts ...string) registrationFiles {
	t.Helper()
	dir := t.TempDir()
	files := registrationFiles{
		ca:         filepath.Join(dir, "ca.crt"),
		serverCert: filepath.Join(dir, "server.crt"),
		serverKey:  filepath.Join(dir, "server.key"),
		clientCert: filepath.Join(dir, "client.crt"),
		clientKey:  filepath.Join(dir, "client.key"),
	}
	serverCert, serverKey := ca.leaf(t, "api", x509.ExtKeyUsageServerAuth, apiHosts...)
	clientCert, clientKey := ca.leaf(t, "runner", x509.ExtKeyUsageClientAuth)
	for path, content := range map[string][]byte{
		files.ca:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}),
		files.serverCert: serverCert,
		files.serverKey:  serverKey,
		files.clientCert: clientCert,
		files.clientKey:  clientKey,
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}
