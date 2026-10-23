package credentials

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
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
