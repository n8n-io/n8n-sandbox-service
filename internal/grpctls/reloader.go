package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"sync"
	"time"
)

// reloadRetryInterval bounds how often a reloader retries files that failed to
// load and have not changed since, so a broken pair costs one load attempt per
// interval rather than one per handshake. A variable so tests can shorten it.
var reloadRetryInterval = time.Second

// reloadWarnInterval bounds how often a reloader repeats the warning for files
// that keep failing to load.
const reloadWarnInterval = time.Minute

// KeyPairReloader reloads a PEM certificate and key from disk when either
// file's mtime changes, so a renewed leaf takes effect on the next handshake
// without restarting the process.
//
// Once a pair has loaded, a failed reload keeps serving it: the files may be
// caught between the cert and the key being replaced, or may be broken, and the
// previous pair stays valid until it expires. The reloader retries on later
// handshakes and logs a warning when the files change and at most once a minute
// otherwise.
type KeyPairReloader struct {
	CertPath, KeyPath string

	mu       sync.Mutex
	cert     *tls.Certificate
	loaded   fileStamp // mtimes of files the current cert came from
	failed   fileStamp // mtimes of files the last failed load read
	lastTry  time.Time
	lastWarn time.Time
}

type fileStamp struct {
	cert, key time.Time
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.cert.Equal(b.cert) && a.key.Equal(b.key)
}

func (k *KeyPairReloader) stamp() (fileStamp, error) {
	stC, err := os.Stat(k.CertPath)
	if err != nil {
		return fileStamp{}, err
	}
	stK, err := os.Stat(k.KeyPath)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{cert: stC.ModTime(), key: stK.ModTime()}, nil
}

// load reads the pair. The stamp is taken first: a change landing in between
// then shows up as a newer stamp on the next call and loads again, instead of
// being recorded as already loaded.
func (k *KeyPairReloader) load() (*tls.Certificate, fileStamp, error) {
	stamp, err := k.stamp()
	if err != nil {
		return nil, fileStamp{}, err
	}
	cert, err := tls.LoadX509KeyPair(k.CertPath, k.KeyPath)
	if err != nil {
		return nil, stamp, err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, stamp, err
		}
	}
	return &cert, stamp, nil
}

// current returns the pair to present, reloading it first when the files changed.
func (k *KeyPairReloader) current() (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	stamp, err := k.stamp()
	if err == nil {
		if k.cert != nil && stamp.equal(k.loaded) {
			return k.cert, nil
		}
		if k.cert != nil && stamp.equal(k.failed) && time.Since(k.lastTry) < reloadRetryInterval {
			return k.cert, nil
		}
		k.lastTry = time.Now()
		var cert *tls.Certificate
		if cert, stamp, err = k.load(); err == nil {
			k.cert, k.loaded, k.failed = cert, stamp, fileStamp{}
			logLoaded(k.CertPath, cert.Leaf)
			return cert, nil
		}
	}

	if k.cert == nil {
		return nil, err
	}
	if !stamp.equal(k.failed) || time.Since(k.lastWarn) >= reloadWarnInterval {
		k.failed = stamp
		k.lastWarn = time.Now()
		slog.Warn("tls certificate reload failed, keeping the previous certificate",
			"path", k.CertPath,
			"serial", CertSerial(k.cert.Leaf),
			"not_after", k.cert.Leaf.NotAfter.UTC().Format(time.RFC3339),
			"error", err)
	}
	return k.cert, nil
}

// GetCertificate implements tls.Config.GetCertificate for the server.
func (k *KeyPairReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return k.current()
}

// GetClientCertificate implements tls.Config.GetClientCertificate for the client.
func (k *KeyPairReloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return k.current()
}

// Prime loads the pair for the first time and fails if it cannot. It also makes
// the certificate visible to CertificateExpiryByCertPath.
func (k *KeyPairReloader) Prime() error {
	if err := k.prime(); err != nil {
		return err
	}
	reloaders.mu.Lock()
	defer reloaders.mu.Unlock()
	reloaders.byFiles[filePair{k.CertPath, k.KeyPath}] = k
	return nil
}

func (k *KeyPairReloader) prime() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	cert, stamp, err := k.load()
	if err != nil {
		return err
	}
	k.cert, k.loaded, k.failed = cert, stamp, fileStamp{}
	logLoaded(k.CertPath, cert.Leaf)
	return nil
}

// reloaderFor returns the process's reloader for a certificate and key file,
// priming one on first use. Credentials built per call or per connection (the
// API's control calls, the runner's registration) share it with the long-lived
// listeners, so they also keep the previous pair when renewed files don't load,
// and don't read the files again for every call.
func reloaderFor(certPath, keyPath string) (*KeyPairReloader, error) {
	reloaders.mu.Lock()
	defer reloaders.mu.Unlock()
	files := filePair{certPath, keyPath}
	if k, ok := reloaders.byFiles[files]; ok {
		return k, nil
	}
	k := &KeyPairReloader{CertPath: certPath, KeyPath: keyPath}
	if err := k.prime(); err != nil {
		return nil, err
	}
	reloaders.byFiles[files] = k
	return k, nil
}

type filePair struct{ cert, key string }

// reloaders holds the primed reloader per pair of files. Lock order: reloaders.mu
// before a reloader's own mutex, never the reverse.
var reloaders = struct {
	mu      sync.Mutex
	byFiles map[filePair]*KeyPairReloader
}{byFiles: map[filePair]*KeyPairReloader{}}

func allReloaders() []*KeyPairReloader {
	reloaders.mu.Lock()
	defer reloaders.mu.Unlock()
	out := make([]*KeyPairReloader, 0, len(reloaders.byFiles))
	for _, k := range reloaders.byFiles {
		out = append(out, k)
	}
	return out
}

// CertificateExpiryByCertPath returns, per certificate file this process presents, when
// the certificate it presents on its next handshake expires. It picks up renewed
// files first, so a long-lived connection that is still on an older certificate
// does not hold the value back.
func CertificateExpiryByCertPath() map[string]time.Time {
	out := map[string]time.Time{}
	for _, k := range allReloaders() {
		cert, err := k.current()
		if err != nil {
			continue
		}
		out[k.CertPath] = cert.Leaf.NotAfter
	}
	return out
}

// loadedCerts remembers the certificate last logged per file, so a reload is
// logged once per process even when credentials are built per connection.
var loadedCerts = struct {
	mu     sync.Mutex
	byPath map[string]string
}{byPath: map[string]string{}}

// logLoaded logs a certificate the first time this process loads it from path
// and reports whether it did.
func logLoaded(path string, leaf *x509.Certificate) bool {
	fp := fingerprint(leaf)
	loadedCerts.mu.Lock()
	prev, seen := loadedCerts.byPath[path]
	if seen && prev == fp {
		loadedCerts.mu.Unlock()
		return false
	}
	loadedCerts.byPath[path] = fp
	loadedCerts.mu.Unlock()

	slog.Info("tls certificate loaded",
		"path", path,
		"subject", leaf.Subject.CommonName,
		"serial", CertSerial(leaf),
		"not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
		"reload", seen)
	return true
}
