// Package credentials resolves the secrets the egress proxy injects and puts
// them on requests. A secret is read from the runner's secret store under the
// owning tenant's directory only; it never reaches the guest, a response or a
// log line.
package credentials

import (
	"bytes"
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
	// Surrounding blanks are trimmed: an upstream drops them from a header
	// value, so an echo of the secret would not match it with them.
	value := []byte(strings.Trim(strings.TrimRight(string(raw), "\r\n"), " \t"))
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

// Masks are the forms of secret a response could echo: the secret itself,
// the header value Inject sets (and a basic credential's token), each also as
// an echo would escape it in JSON or a URL.
func Masks(c runnerruntime.EgressCredential, secret []byte) [][]byte {
	if len(secret) == 0 {
		return nil
	}
	h := http.Header{}
	Inject(h, c, secret)
	forms := []string{string(secret)}
	if v := h.Get(HeaderName(c)); v != string(secret) {
		forms = append(forms, v)
		if c.Scheme == runnerruntime.CredentialBasic {
			forms = append(forms, strings.TrimPrefix(v, "Basic "))
		}
	}
	seen := map[string]bool{}
	var masks [][]byte
	for _, f := range forms {
		for _, v := range escapedForms(f) {
			if !seen[v] {
				seen[v] = true
				masks = append(masks, []byte(v))
			}
		}
	}
	return masks
}

// escapedForms is s as an echo might write it: as is, JSON-escaped ("\/",
// "\u002b" in either case) and percent-encoded (either case, space as %20 or
// "+").
func escapedForms(s string) []string {
	var jsonSlash, jsonLower, jsonUpper, pctUpper, pctLower, pctPlus strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		unreserved := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0
		if c == '/' {
			jsonSlash.WriteString(`\/`)
		} else {
			jsonSlash.WriteByte(c)
		}
		if strings.IndexByte("+/=&<>", c) >= 0 {
			fmt.Fprintf(&jsonLower, `\u%04x`, c)
			fmt.Fprintf(&jsonUpper, `\u%04X`, c)
		} else {
			jsonLower.WriteByte(c)
			jsonUpper.WriteByte(c)
		}
		switch {
		case unreserved:
			pctUpper.WriteByte(c)
			pctLower.WriteByte(c)
			pctPlus.WriteByte(c)
		case c == ' ':
			pctUpper.WriteString("%20")
			pctLower.WriteString("%20")
			pctPlus.WriteByte('+')
		default:
			fmt.Fprintf(&pctUpper, "%%%02X", c)
			fmt.Fprintf(&pctLower, "%%%02x", c)
			fmt.Fprintf(&pctPlus, "%%%02X", c)
		}
	}
	return []string{s, jsonSlash.String(), jsonLower.String(), jsonUpper.String(), pctUpper.String(), pctLower.String(), pctPlus.String()}
}

// ScrubHeader masks every mask in h's values.
func ScrubHeader(h http.Header, masks [][]byte) {
	for k, vs := range h {
		for i, v := range vs {
			b := []byte(v)
			if maskAll(b, masks) {
				h[k][i] = string(b)
			}
		}
	}
}

// Scrub returns r with every mask overwritten by '*' of the same length, so
// a response keeps its length. It holds back only a tail that could begin a
// mask, so a stream is not delayed past the bytes it must check.
func Scrub(r io.Reader, masks [][]byte) io.Reader {
	if len(masks) == 0 {
		return r
	}
	return &scrubber{r: r, masks: masks, buf: make([]byte, 32<<10)}
}

type scrubber struct {
	r       io.Reader
	masks   [][]byte
	buf     []byte
	pending []byte // read, masked, possibly the start of a mask
	out     []byte // ready to hand out
	err     error
}

func (s *scrubber) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			// Nothing more will arrive: a held-back tail can no longer
			// become a mask.
			s.out, s.pending = s.pending, nil
			if len(s.out) == 0 {
				return 0, s.err
			}
			break
		}
		n, err := s.r.Read(s.buf)
		s.err = err
		s.pending = append(s.pending, s.buf[:n]...)
		maskAll(s.pending, s.masks)
		keep := heldBack(s.pending, s.masks)
		s.out = append([]byte(nil), s.pending[:len(s.pending)-keep]...)
		s.pending = append([]byte(nil), s.pending[len(s.pending)-keep:]...)
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// maskAll overwrites each mask in b and reports whether it found one.
func maskAll(b []byte, masks [][]byte) bool {
	found := false
	for _, m := range masks {
		for i := 0; len(m) > 0; {
			j := bytes.Index(b[i:], m)
			if j < 0 {
				break
			}
			for k := i + j; k < i+j+len(m); k++ {
				b[k] = '*'
			}
			i, found = i+j+len(m), true
		}
	}
	return found
}

// heldBack is the length of b's longest suffix that is a proper prefix of a
// mask.
func heldBack(b []byte, masks [][]byte) int {
	keep := 0
	for _, m := range masks {
		for n := min(len(m)-1, len(b)); n > keep; n-- {
			if bytes.HasSuffix(b, m[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}
