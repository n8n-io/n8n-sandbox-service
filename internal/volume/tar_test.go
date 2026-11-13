package volume

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/safetar"
)

func testTar(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range headers {
		body := hdr.Name
		if hdr.Typeflag != tar.TypeReg {
			body = ""
		}
		hdr.Size = int64(len(body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body %q: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func TestTarPolicyKeepsLinksAndExecBits(t *testing.T) {
	body := testTar(t,
		tar.Header{Name: "pkg/cli.js", Typeflag: tar.TypeReg, Mode: 0o4755},
		tar.Header{Name: "pkg/data", Typeflag: tar.TypeReg, Mode: 0o600},
		tar.Header{Name: ".bin/cli", Typeflag: tar.TypeSymlink, Linkname: "../pkg/cli.js"},
		tar.Header{Name: "abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/hostname"},
	)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	stats, err := safetar.Extract(bytes.NewReader(body), root, TarPolicy(4, 18, int64(len(body))))
	if err != nil {
		t.Fatalf("Extract() failed: %v", err)
	}
	if stats != (safetar.Stats{Entries: 4, Files: 2, Bytes: 18, DecompressedBytes: int64(len(body))}) {
		t.Fatalf("stats = %+v", stats)
	}
	for name, want := range map[string]os.FileMode{"pkg/cli.js": 0o755, "pkg/data": 0o644} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s has mode %v, want %v", name, info.Mode(), want)
		}
	}
	for name, want := range map[string]string{".bin/cli": "../pkg/cli.js", "abs": "/etc/hostname"} {
		if got, err := os.Readlink(filepath.Join(dir, name)); err != nil || got != want {
			t.Errorf("link %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestTarPolicyAppliesTheCallersCaps(t *testing.T) {
	body := testTar(t,
		tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "b", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	size := int64(len(body))
	for name, policy := range map[string]safetar.Policy{
		"entries":            TarPolicy(1, 2, size),
		"bytes":              TarPolicy(2, 1, size),
		"decompressed bytes": TarPolicy(2, 2, size-1),
	} {
		if _, err := safetar.Validate(bytes.NewReader(body), policy); !errors.Is(err, safetar.ErrTooLarge) {
			t.Errorf("%s cap: got %v, want %v", name, err, safetar.ErrTooLarge)
		}
	}
	if _, err := safetar.Validate(bytes.NewReader(body), TarPolicy(2, 2, size)); err != nil {
		t.Errorf("at the caps: %v", err)
	}
}

func TestTarPolicyRefusesPathsNoSystemCallTakes(t *testing.T) {
	deep := func(length int) string { return strings.Repeat("d/", length/2-1) + "ab" }
	for name, tt := range map[string]struct {
		header tar.Header
		want   error
	}{
		"a name of 4095 bytes":        {tar.Header{Name: deep(4094) + "c", Typeflag: tar.TypeDir}, nil},
		"a name of 4096 bytes":        {tar.Header{Name: deep(4096), Typeflag: tar.TypeDir}, safetar.ErrInvalid},
		"a link target of 4096 bytes": {tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: deep(4096)}, safetar.ErrInvalid},
	} {
		body := testTar(t, tt.header)
		if _, err := safetar.Validate(bytes.NewReader(body), TarPolicy(10, 0, 0)); !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", name, err, tt.want)
		}
	}
}

func TestTarPolicyRefusesHardLinks(t *testing.T) {
	body := testTar(t,
		tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"},
	)
	if _, err := safetar.Validate(bytes.NewReader(body), TarPolicy(10, 10, 0)); !errors.Is(err, safetar.ErrInvalid) {
		t.Errorf("got %v, want %v", err, safetar.ErrInvalid)
	}
}
