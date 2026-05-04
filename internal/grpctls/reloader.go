package grpctls

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// KeyPairReloader reloads PEM certificate and key from disk when either file's
// mtime changes (leaf rotation without replacing the process).
type KeyPairReloader struct {
	CertPath, KeyPath string

	mu       sync.RWMutex
	cert     tls.Certificate
	maxMtime time.Time
}

func (k *KeyPairReloader) maybeReload() error {
	stC, errC := os.Stat(k.CertPath)
	stK, errK := os.Stat(k.KeyPath)
	if errC != nil {
		return errC
	}
	if errK != nil {
		return errK
	}
	mt := stC.ModTime()
	if stK.ModTime().After(mt) {
		mt = stK.ModTime()
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.maxMtime.IsZero() && !mt.After(k.maxMtime) {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(k.CertPath, k.KeyPath)
	if err != nil {
		return err
	}
	k.cert = cert
	k.maxMtime = mt
	return nil
}

// GetCertificate implements tls.Config.GetCertificate for the server.
func (k *KeyPairReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if err := k.maybeReload(); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	c := k.cert
	return &c, nil
}

// GetClientCertificate implements tls.Config.GetClientCertificate for the client.
func (k *KeyPairReloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if err := k.maybeReload(); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	c := k.cert
	return &c, nil
}

// Prime forces an initial load (for tests and startup).
func (k *KeyPairReloader) Prime() error {
	k.mu.Lock()
	k.maxMtime = time.Time{}
	k.mu.Unlock()
	return k.maybeReload()
}
