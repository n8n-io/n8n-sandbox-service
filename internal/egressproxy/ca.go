package egressproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

// CA signs the certificates the proxy presents to guests for the hosts it
// inspects. Its key stays in the runner process; guests get the certificate
// alone, to trust.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// leafLifetime bounds a minted certificate; maxLeaves bounds the cache.
const (
	leafLifetime = 7 * 24 * time.Hour
	maxLeaves    = 4096
)

// NewCA generates a CA held in memory only, so its key never touches disk.
// Guests learn its certificate at create, so a runner restart, which deletes
// every sandbox, is also what rotates it.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "n8n sandbox egress proxy CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		leaves:  map[string]*tls.Certificate{},
	}, nil
}

// LoadCA reads a CA certificate and key from PEM files, for operators who
// want one CA across runners.
func LoadCA(certFile, keyFile string) (*CA, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("egress proxy CA key must be ECDSA")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("egress proxy CA certificate is not a CA")
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, certPEM: certPEM, key: key, leaves: map[string]*tls.Certificate{}}, nil
}

// CertPEM is the CA certificate guests trust.
func (c *CA) CertPEM() []byte { return c.certPEM }

// leaf returns a certificate for host, minting and caching it on first use.
func (c *CA) leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok && time.Until(l.Leaf.NotAfter) > time.Hour {
		return l, nil
	}
	// Bounded: minting again is cheap, holding every host ever named is not.
	if len(c.leaves) >= maxLeaves {
		clear(c.leaves)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mint certificate for %s: %w", host, err)
	}
	leafCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leafCert}
	c.leaves[host] = l
	return l, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}
