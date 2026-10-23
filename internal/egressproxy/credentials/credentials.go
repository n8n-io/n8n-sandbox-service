// Package credentials resolves the secrets the egress proxy injects and puts
// them on requests. A secret is read from the runner's secret store under the
// owning tenant's directory only; it never reaches the guest, a response or a
// log line.
package credentials

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// refreshAfter is how long a read secret is reused; a rotated file is picked
// up within it.
const refreshAfter = 30 * time.Second

// maxSecretBytes bounds a secret file.
const maxSecretBytes = 16 << 10

// ErrUnavailable is any failure to read a secret. The cause is logged by the
// caller with the reference, never with the secret.
var ErrUnavailable = errors.New("credential unavailable")

var tenantRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store reads secrets from <dir>/<tenant>/<secret_ref>.
type Store struct {
	root *os.Root

	mu    sync.Mutex
	cache map[string]cached
	now   func() time.Time
}

type cached struct {
	value []byte
	at    time.Time
}

// Open opens the secret store at dir. os.Root keeps every read inside it,
// whatever symlinks the directory holds.
func Open(dir string) (*Store, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, cache: map[string]cached{}, now: time.Now}, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.root.Close() }

// Secret returns tenant's secret ref. The reference's form was validated at
// create; it is checked again here because this is where it becomes a path.
func (s *Store) Secret(tenant, ref string) ([]byte, error) {
	if !tenantRe.MatchString(tenant) || ref == "" || strings.Contains(ref, "..") || strings.HasPrefix(ref, "/") || strings.ContainsRune(ref, 0) {
		return nil, ErrUnavailable
	}
	key := tenant + "/" + ref
	s.mu.Lock()
	if c, ok := s.cache[key]; ok && s.now().Sub(c.at) < refreshAfter {
		s.mu.Unlock()
		return c.value, nil
	}
	s.mu.Unlock()

	// The tenant's directory is a root of its own, so not even a symlink the
	// store holds leads from one tenant's secrets to another's.
	tenantRoot, err := s.root.OpenRoot(tenant)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer tenantRoot.Close()
	f, err := tenantRoot.Open(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSecretBytes {
		return nil, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil || len(raw) > maxSecretBytes {
		return nil, ErrUnavailable
	}
	value := []byte(strings.TrimRight(string(raw), "\r\n"))
	if len(value) == 0 || strings.ContainsAny(string(value), "\r\n\x00") {
		return nil, ErrUnavailable // a header value cannot carry it
	}
	s.mu.Lock()
	s.cache[key] = cached{value: value, at: s.now()}
	s.mu.Unlock()
	return value, nil
}

// HeaderName is the header c sets.
func HeaderName(c runnerruntime.EgressCredential) string {
	if c.Scheme == runnerruntime.CredentialHeader {
		return http.CanonicalHeaderKey(c.Header)
	}
	return "Authorization"
}

// Inject replaces whatever the guest sent in c's header with the secret.
// Proxy-Authorization never goes upstream either.
func Inject(h http.Header, c runnerruntime.EgressCredential, secret []byte) {
	h.Del("Proxy-Authorization")
	name := HeaderName(c)
	h.Del(name)
	switch c.Scheme {
	case runnerruntime.CredentialBasic:
		h.Set(name, "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Username+":"+string(secret))))
	case runnerruntime.CredentialHeader:
		h.Set(name, string(secret))
	default:
		h.Set(name, "Bearer "+string(secret))
	}
}
