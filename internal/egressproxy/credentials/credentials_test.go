package credentials

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func writeSecret(t *testing.T, dir, rel, value string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A tenant reads its own secrets only: no reference, symlink or traversal
// reaches another tenant's or anything outside the store.
func TestSecretStaysInTenant(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeSecret(t, dir, "t1/factory/git-token", "tok-1\n")
	writeSecret(t, dir, "t2/factory/git-token", "tok-2")
	writeSecret(t, outside, "stolen", "nope")
	if err := os.Symlink(filepath.Join(outside, "stolen"), filepath.Join(dir, "t1", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "t2", "factory", "git-token"), filepath.Join(dir, "t1", "cross")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if v, err := s.Secret("t1", "factory/git-token"); err != nil || string(v) != "tok-1" {
		t.Fatalf("own secret = %q, %v", v, err)
	}
	for _, tc := range [][2]string{
		{"t1", "../t2/factory/git-token"},
		{"t1", "escape"},
		{"../t2", "factory/git-token"},
		{"t1/../t2", "factory/git-token"},
		{"t1", "/etc/passwd"},
		{"t1", "missing"},
		{"t1", "cross"}, // a symlink to another tenant's secret
		{"", "factory/git-token"},
	} {
		if _, err := s.Secret(tc[0], tc[1]); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Secret(%q, %q) = %v, want ErrUnavailable", tc[0], tc[1], err)
		}
	}
}

func TestSecretRefreshesAfterRotation(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "t1/k", "old")
	s, _ := Open(dir)
	defer s.Close()
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	if v, _ := s.Secret("t1", "k"); string(v) != "old" {
		t.Fatal(string(v))
	}
	writeSecret(t, dir, "t1/k", "new")
	if v, _ := s.Secret("t1", "k"); string(v) != "old" {
		t.Fatal("cache not used")
	}
	now = now.Add(refreshAfter)
	if v, _ := s.Secret("t1", "k"); string(v) != "new" {
		t.Fatalf("after refresh = %q", v)
	}
}

func TestSecretRefusesMultiLineValues(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "t1/k", "a\nInjected: yes")
	s, _ := Open(dir)
	defer s.Close()
	if _, err := s.Secret("t1", "k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// Whatever the guest sent in the credential's header is replaced, and
// Proxy-Authorization never goes upstream.
func TestInject(t *testing.T) {
	for _, tc := range []struct {
		cred       runnerruntime.EgressCredential
		name, want string
	}{
		{runnerruntime.EgressCredential{Scheme: runnerruntime.CredentialBearer}, "Authorization", "Bearer s3cret"},
		{runnerruntime.EgressCredential{Scheme: runnerruntime.CredentialBasic, Username: "factory-agent"}, "Authorization", "Basic ZmFjdG9yeS1hZ2VudDpzM2NyZXQ="},
		{runnerruntime.EgressCredential{Scheme: runnerruntime.CredentialHeader, Header: "x-api-key"}, "X-Api-Key", "s3cret"},
	} {
		h := http.Header{}
		h.Add(tc.name, "guest-value")
		h.Add(tc.name, "second")
		h.Set("Proxy-Authorization", "Basic x")
		Inject(h, tc.cred, []byte("s3cret"))
		if got := h.Values(tc.name); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%v: %s = %v", tc.cred.Scheme, tc.name, got)
		}
		if h.Get("Proxy-Authorization") != "" {
			t.Error("Proxy-Authorization forwarded")
		}
	}
}

// A response echoing a credential, whole or split across reads, comes out
// masked and the same length; a stream is not held back.
func TestScrub(t *testing.T) {
	c := runnerruntime.EgressCredential{Host: "git.internal", SecretRef: "t", Scheme: runnerruntime.CredentialBasic, Username: "factory"}
	masks := Masks(c, []byte("s3cret-token"))
	b64 := base64.StdEncoding.EncodeToString([]byte("factory:s3cret-token"))
	in := "token=s3cret-token auth=Basic " + b64 + " again s3cret-token."
	for name, r := range map[string]io.Reader{
		"whole":    strings.NewReader(in),
		"one byte": iotest.OneByteReader(strings.NewReader(in)),
	} {
		out, err := io.ReadAll(Scrub(r, masks))
		if err != nil || len(out) != len(in) || strings.Contains(string(out), "s3cret") || strings.Contains(string(out), b64) {
			t.Errorf("%s: %q %v", name, out, err)
		}
	}
	// An event that cannot begin a mask is handed out at once.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("data: {}\n\n")) }()
	got := make([]byte, 64)
	if n, _ := Scrub(pr, masks).Read(got); string(got[:n]) != "data: {}\n\n" {
		t.Fatalf("stream held back: %q", got[:n])
	}
	_ = pw.Close()

	// An echo that escapes the secret as JSON or a URL would.
	slashy := Masks(runnerruntime.EgressCredential{Scheme: runnerruntime.CredentialHeader, Header: "x-api-key"}, []byte("ab/c+d=e"))
	for _, echo := range []string{`{"key":"ab\/c+d=e"}`, `{"key":"ab\u002fc\u002bd\u003de"}`, `?key=ab%2Fc%2Bd%3De`, `?key=ab%2fc%2bd%3de`} {
		out, _ := io.ReadAll(Scrub(strings.NewReader(echo), slashy))
		if strings.Contains(string(out), "ab") {
			t.Errorf("escaped echo %q came out %q", echo, out)
		}
	}

	h := http.Header{"X-Echo": {"Bearer s3cret-token"}}
	ScrubHeader(h, masks)
	if strings.Contains(h.Get("X-Echo"), "s3cret") {
		t.Fatalf("header %q", h.Get("X-Echo"))
	}
}

// FuzzScrub: the length never changes and no mask survives, however the
// body is split.
func FuzzScrub(f *testing.F) {
	f.Add([]byte("xx s3cret s3c s3cret"), uint8(3))
	f.Fuzz(func(t *testing.T, body []byte, chunk uint8) {
		masks := [][]byte{[]byte("s3cret"), []byte("cret-s")}
		r := io.Reader(bytes.NewReader(body))
		if chunk > 0 {
			r = &chunked{b: body, n: int(chunk)}
		}
		out, _ := io.ReadAll(Scrub(r, masks))
		if len(out) != len(body) {
			t.Fatalf("length %d, want %d", len(out), len(body))
		}
		for _, m := range masks {
			if bytes.Contains(out, m) {
				t.Fatalf("mask %q survived in %q", m, out)
			}
		}
	})
}

type chunked struct {
	b []byte
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := min(c.n, len(c.b), len(p))
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}

// Blanks around a secret are trimmed, so an upstream's echo, which drops
// them, still matches its mask.
func TestSecretTrimsBlanks(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "t1/k", " \ttok \n")
	s, _ := Open(dir)
	defer s.Close()
	if v, err := s.Secret("t1", "k"); err != nil || string(v) != "tok" {
		t.Fatalf("secret %q %v", v, err)
	}
}
