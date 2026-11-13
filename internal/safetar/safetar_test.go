package safetar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"golang.org/x/sys/unix"
)

type tarEntry struct {
	hdr  tar.Header
	body string
}

func tarFile(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: body}
}

func tarDir(name string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}

func tarSymlink(name, target string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

func writeEntries(t *testing.T, tw *tar.Writer, entries []tarEntry) {
	t.Helper()
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write tar header %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("write tar body %q: %v", hdr.Name, err)
		}
	}
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeEntries(t, tw, entries)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// claimTar ends with the header of a file "outside.txt" of size bytes, and no
// content.
func claimTar(t *testing.T, size int64, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeEntries(t, tw, entries)
	if err := tw.WriteHeader(&tar.Header{Name: "outside.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// sparseTar builds a pax sparse file (format 1.0) of realSize bytes, all hole,
// by hand, since tar.Writer does not write sparse files: a pax header written
// as a regular entry with its type patched afterwards, then the entry whose
// data starts with an empty sparse map.
func sparseTar(t *testing.T, name string, realSize int64) []byte {
	t.Helper()
	record := func(key, value string) string {
		rest := " " + key + "=" + value + "\n"
		n := len(rest) + len(strconv.Itoa(len(rest)))
		if len(strconv.Itoa(n)) > len(strconv.Itoa(len(rest))) {
			n = len(rest) + len(strconv.Itoa(n))
		}
		return strconv.Itoa(n) + rest
	}
	pax := record("GNU.sparse.major", "1") + record("GNU.sparse.minor", "0") +
		record("GNU.sparse.name", name) + record("GNU.sparse.realsize", strconv.FormatInt(realSize, 10))
	sparseMap := make([]byte, 512)
	copy(sparseMap, "0\n")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct {
		name string
		data []byte
	}{{"PaxHeaders/" + name, []byte(pax)}, {"GNUSparseFile/" + name, sparseMap}} {
		hdr := &tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.data)), Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatalf("write data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	data := buf.Bytes()
	header := data[:512]
	header[156] = tar.TypeXHeader
	copy(header[148:156], "        ")
	var sum int64
	for _, b := range header {
		sum += int64(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return data
}

// newTarget returns an empty target directory and its parent.
func newTarget(t *testing.T) (base, target string) {
	t.Helper()
	// Resolve symlinks so the path is stable on macOS (/var vs /private/var).
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	target = filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	return base, target
}

// unpackBoth fails the test unless Extract and Validate agree.
func unpackBoth(t *testing.T, target string, body []byte, p Policy) (Stats, error) {
	t.Helper()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	stats, err := Extract(bytes.NewReader(body), root, p)
	vStats, vErr := Validate(bytes.NewReader(body), p)
	if stats != vStats || fmt.Sprint(err) != fmt.Sprint(vErr) {
		t.Fatalf("Extract and Validate disagree:\n  Extract:  %+v %v\n  Validate: %+v %v", stats, err, vStats, vErr)
	}
	return stats, err
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// umasked is perm less this process's umask.
func umasked(t *testing.T, perm os.FileMode, dir bool) os.FileMode {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reference")
	var err error
	if dir {
		err = os.Mkdir(path, perm)
	} else {
		err = os.WriteFile(path, nil, perm)
	}
	if err != nil {
		t.Fatalf("create reference: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat reference: %v", err)
	}
	return fi.Mode().Perm()
}

func TestExtractWritesFiles(t *testing.T) {
	for _, compress := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%v", compress), func(t *testing.T) {
			_, target := newTarget(t)
			if err := os.WriteFile(filepath.Join(target, "x.txt"), []byte("old content"), 0o600); err != nil {
				t.Fatalf("write existing file: %v", err)
			}

			setuid := tarFile("deep/nested/y.txt", "yy")
			setuid.hdr.Mode = 0o4755
			// "./" and "./"-prefixed names are what `tar -C dir .` writes.
			plain := buildTar(t,
				tarEntry{hdr: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "global"}}},
				tarDir("./"),
				tarEntry{hdr: tar.Header{Name: "./empty/", Typeflag: tar.TypeDir, Mode: 0o700}},
				tarFile("./x.txt", "new"),
				setuid,
				tarFile(strings.Repeat("long/", 40)+"name.txt", ""),
			)
			body := plain
			if compress {
				body = gzipped(t, plain)
			}

			stats, err := unpackBoth(t, target, body, Policy{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if stats != (Stats{Entries: 6, Files: 3, Bytes: 5, DecompressedBytes: int64(len(plain))}) {
				t.Fatalf("unexpected stats: %+v", stats)
			}
			if got := readString(t, filepath.Join(target, "x.txt")); got != "new" {
				t.Fatalf("existing file not overwritten: %q", got)
			}
			fi, err := os.Stat(filepath.Join(target, "x.txt"))
			if err != nil {
				t.Fatalf("stat existing file: %v", err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("existing file should keep its mode 0600, got %v", fi.Mode().Perm())
			}
			if got := readString(t, filepath.Join(target, "deep", "nested", "y.txt")); got != "yy" {
				t.Fatalf("nested file: %q", got)
			}
			fi, err = os.Stat(filepath.Join(target, "deep", "nested", "y.txt"))
			if err != nil {
				t.Fatalf("stat nested file: %v", err)
			}
			if fi.Mode()&(os.ModeSetuid|0o111) != 0 {
				t.Fatalf("archive mode was applied: %v", fi.Mode())
			}
			fi, err = os.Stat(filepath.Join(target, "empty"))
			if err != nil || !fi.IsDir() {
				t.Fatalf("empty directory not created: %v", err)
			}
			if want := umasked(t, 0o755, true); fi.Mode().Perm() != want {
				t.Fatalf("archive directory mode was applied: got %v, want %v", fi.Mode().Perm(), want)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	truncated := buildTar(t, tarFile("big.txt", strings.Repeat("x", 100)))[:512+10]
	validGzip := gzipped(t, buildTar(t, tarFile("a.txt", "a")))
	badChecksum := bytes.Clone(validGzip)
	badChecksum[len(badChecksum)-8] ^= 0xff
	symlinks := Policy{AllowSymlinks: true, MaxEntries: 100}
	local := Policy{AllowSymlinks: true, LocalSymlinks: true, MaxEntries: 100}

	tests := []struct {
		name   string
		policy Policy
		// Every refused entry would write "outside.txt" in base or the target.
		body     func(t *testing.T, base string) []byte
		gzipOnly bool
		want     error
		entry    string
		stream   bool
	}{
		{
			name: "absolute path",
			body: func(t *testing.T, base string) []byte {
				return buildTar(t, tarFile(filepath.Join(base, "outside.txt"), "x"))
			},
			want:  ErrInvalid,
			entry: "<base>/outside.txt",
		},
		{
			name:  "parent directory",
			body:  func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("../outside.txt", "x")) },
			want:  ErrInvalid,
			entry: "../outside.txt",
		},
		{
			name:  "parent directory that stays inside",
			body:  func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("a/../outside.txt", "x")) },
			want:  ErrInvalid,
			entry: "a/../outside.txt",
		},
		{
			name:  "empty name",
			body:  func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("", "x")) },
			want:  ErrInvalid,
			entry: "",
		},
		{
			name: "symlink not allowed",
			body: func(t *testing.T, base string) []byte {
				return buildTar(t, tarSymlink("outside.txt", base))
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name: "hard link",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarFile("a.txt", "a"), tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeLink, Linkname: "a.txt"}})
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name:   "hard link with symlinks allowed",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarFile("a.txt", "a"), tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeLink, Linkname: "a.txt"}})
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name: "character device",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}})
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name:   "block device",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeBlock, Devmajor: 8}})
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name: "fifo",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeFifo}})
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name:  "sparse file",
			body:  func(t *testing.T, _ string) []byte { return sparseTar(t, "outside.txt", 1<<30) },
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name:   "empty symlink target",
			policy: symlinks,
			body:   func(t *testing.T, _ string) []byte { return buildTar(t, tarSymlink("outside.txt", "")) },
			want:   ErrInvalid,
			entry:  "outside.txt",
		},
		{
			name:   "file through an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("link", "."), tarFile("link/outside.txt", "x"))
			},
			want:  ErrInvalid,
			entry: "link/outside.txt",
		},
		{
			name:   "file through an earlier symlink, uncleaned",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("d/link", ".."), tarFile("./d//link/./outside.txt", "x"))
			},
			want:  ErrInvalid,
			entry: "./d//link/./outside.txt",
		},
		{
			name:   "directory through an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("link", "."), tarDir("link/outside.txt/"))
			},
			want:  ErrInvalid,
			entry: "link/outside.txt/",
		},
		{
			name:   "symlink through an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("link", "."), tarSymlink("link/outside.txt", "x"))
			},
			want:  ErrInvalid,
			entry: "link/outside.txt",
		},
		{
			name:   "file replacing an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("link", "outside.txt"), tarFile("link", "x"))
			},
			want:  ErrInvalid,
			entry: "link",
		},
		{
			name:   "directory replacing an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarDir("d/"), tarSymlink("link", "d"), tarDir("link/"))
			},
			want:  ErrInvalid,
			entry: "link/",
		},
		{
			name:   "symlink replacing an earlier symlink",
			policy: symlinks,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("link", "a"), tarSymlink("./link", "outside.txt"))
			},
			want:  ErrInvalid,
			entry: "./link",
		},
		{
			name:   "absolute symlink target",
			policy: local,
			body:   func(t *testing.T, base string) []byte { return buildTar(t, tarSymlink("outside.txt", base)) },
			want:   ErrInvalid,
			entry:  "outside.txt",
		},
		{
			name:   "symlink target with ..",
			policy: local,
			body:   func(t *testing.T, _ string) []byte { return buildTar(t, tarSymlink("outside.txt", "a/../b")) },
			want:   ErrInvalid,
			entry:  "outside.txt",
		},
		{
			// y/.. is the parent of the target directory once y resolves.
			name:   "symlink chain out of the tree",
			policy: local,
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("y", "."), tarSymlink("outside.txt", "y/.."))
			},
			want:  ErrInvalid,
			entry: "outside.txt",
		},
		{
			name:   "invalid tar header",
			body:   func(t *testing.T, _ string) []byte { return bytes.Repeat([]byte("x"), 512) },
			want:   ErrInvalid,
			stream: true,
		},
		{
			name:  "truncated file content",
			body:  func(t *testing.T, _ string) []byte { return truncated },
			want:  ErrInvalid,
			entry: "big.txt",
		},
		{
			name: "invalid gzip",
			body: func(t *testing.T, _ string) []byte {
				return append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("x"), 20)...)
			},
			gzipOnly: true,
			want:     ErrInvalid,
			stream:   true,
		},
		{
			name:     "corrupt gzip footer",
			body:     func(t *testing.T, _ string) []byte { return badChecksum },
			gzipOnly: true,
			want:     ErrInvalid,
			stream:   true,
		},
		{
			name:     "truncated gzip footer",
			body:     func(t *testing.T, _ string) []byte { return validGzip[:len(validGzip)-8] },
			gzipOnly: true,
			want:     ErrInvalid,
			stream:   true,
		},
		{
			name:   "more entries than MaxEntries",
			policy: Policy{MaxEntries: 3},
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t,
					tarEntry{hdr: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}},
					tarDir("a/"), tarDir("b/"), tarFile("outside.txt", "x"))
			},
			want:   ErrTooLarge,
			stream: true,
		},
		{
			name:   "more bytes than MaxBytes",
			policy: Policy{MaxBytes: 5},
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarFile("a.txt", "abc"), tarFile("outside.txt", "abc"))
			},
			want:  ErrTooLarge,
			entry: "outside.txt",
		},
		{
			// Reading the missing content would fail with ErrInvalid instead.
			name:   "size past MaxBytes, content missing",
			policy: Policy{MaxBytes: 1 << 20},
			body:   func(t *testing.T, _ string) []byte { return claimTar(t, 1<<40) },
			want:   ErrTooLarge,
			entry:  "outside.txt",
		},
		{
			// Adding the sizes would overflow.
			name:   "size that would overflow the total",
			policy: Policy{MaxBytes: math.MaxInt64},
			body:   func(t *testing.T, _ string) []byte { return claimTar(t, math.MaxInt64, tarFile("a.txt", "abc")) },
			want:   ErrTooLarge,
			entry:  "outside.txt",
		},
	}

	for _, tc := range tests {
		for _, compress := range []bool{false, true} {
			if compress && tc.gzipOnly {
				continue
			}
			t.Run(fmt.Sprintf("%s/gzip=%v", tc.name, compress || tc.gzipOnly), func(t *testing.T) {
				base, target := newTarget(t)
				body := tc.body(t, base)
				if compress {
					body = gzipped(t, body)
				}

				_, err := unpackBoth(t, target, body, tc.policy)
				if !errors.Is(err, tc.want) {
					t.Fatalf("expected %v, got %v", tc.want, err)
				}
				if tc.want == ErrTooLarge && errors.Is(err, ErrInvalid) {
					t.Fatalf("a cap is not an invalid archive: %v", err)
				}
				var entryErr *EntryError
				switch {
				case tc.stream:
					if errors.As(err, &entryErr) {
						t.Fatalf("expected a stream error, got %v", err)
					}
				case !errors.As(err, &entryErr):
					t.Fatalf("expected an entry error, got %v", err)
				case entryErr.Name != strings.ReplaceAll(tc.entry, "<base>", base):
					t.Fatalf("expected entry %q, got %q", tc.entry, entryErr.Name)
				}
				for _, dir := range []string{base, target} {
					if _, statErr := os.Lstat(filepath.Join(dir, "outside.txt")); !os.IsNotExist(statErr) {
						t.Fatalf("a refused entry was written to %s, stat err: %v", dir, statErr)
					}
				}
			})
		}
	}
}

func TestSymlinks(t *testing.T) {
	base, target := newTarget(t)
	body := buildTar(t,
		tarDir("d/"),
		tarFile("f.txt", "f"),
		tarSymlink("d/up", "../f.txt"),
		tarSymlink("nested/abs", base),
	)

	stats, err := unpackBoth(t, target, body, Policy{AllowSymlinks: true, MaxEntries: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats != (Stats{Entries: 4, Files: 1, Bytes: 1, DecompressedBytes: int64(len(body))}) {
		t.Fatalf("symlinks are not files: %+v", stats)
	}
	for name, want := range map[string]string{"d/up": "../f.txt", "nested/abs": base} {
		got, err := os.Readlink(filepath.Join(target, name))
		if err != nil || got != want {
			t.Fatalf("symlink %s: got %q, %v; want %q", name, got, err, want)
		}
	}

	_, target = newTarget(t)
	_, err = unpackBoth(t, target, body, Policy{})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "only regular files and directories are allowed") {
		t.Fatalf("expected the symlink to be refused, got %v", err)
	}

	_, target = newTarget(t)
	local := buildTar(t,
		tarDir("d/c/"), tarFile("f.txt", "f"), tarFile("d/c/x", "x"),
		tarSymlink("a", "f.txt"), tarSymlink("d/b", "./c//x"), tarSymlink("dot", "."))
	if _, err := unpackBoth(t, target, local, Policy{AllowSymlinks: true, LocalSymlinks: true, MaxEntries: 10}); err != nil {
		t.Fatalf("LocalSymlinks refused a local target: %v", err)
	}
	for name, want := range map[string]string{"a": "f.txt", "d/b": "d/c/x", "dot": "."} {
		got, err := filepath.EvalSymlinks(filepath.Join(target, name))
		if err != nil || got != filepath.Join(target, want) {
			t.Fatalf("symlink %s resolves to %q, %v; want %q", name, got, err, filepath.Join(target, want))
		}
	}
}

// Without the check, O_TRUNC would rewrite the file behind the link.
func TestNoWriteThroughEarlierSymlink(t *testing.T) {
	_, target := newTarget(t)
	body := buildTar(t,
		tarFile("real.txt", "keep"),
		tarSymlink("link", "real.txt"),
		tarFile("link", "overwritten"),
	)

	_, err := unpackBoth(t, target, body, Policy{AllowSymlinks: true, MaxEntries: 10})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "symbolic link from the archive") {
		t.Fatalf("expected the write through the link to be refused, got %v", err)
	}
	if got := readString(t, filepath.Join(target, "real.txt")); got != "keep" {
		t.Fatalf("file behind the link was written: %q", got)
	}
}

func TestExecBit(t *testing.T) {
	modes := map[string]int64{
		"plain":      0o644,
		"exec":       0o755,
		"group-exec": 0o610,
		"setuid":     0o4755,
		"setgid":     0o2755,
		"sticky":     0o1777,
	}
	var entries []tarEntry
	for name, mode := range modes {
		e := tarFile(name, "x")
		e.hdr.Mode = mode
		entries = append(entries, e)
	}
	existing := tarFile("existing", "x")
	existing.hdr.Mode = 0o755
	body := buildTar(t, append(entries, existing)...)

	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
			_, target := newTarget(t)
			if err := os.WriteFile(filepath.Join(target, "existing"), nil, 0o600); err != nil {
				t.Fatalf("write existing file: %v", err)
			}
			if _, err := unpackBoth(t, target, body, Policy{KeepExecBit: keep}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for name, mode := range modes {
				want := umasked(t, 0o644, false)
				if keep && mode&0o111 != 0 {
					want = umasked(t, 0o755, false)
				}
				fi, err := os.Stat(filepath.Join(target, name))
				if err != nil {
					t.Fatalf("stat %s: %v", name, err)
				}
				if fi.Mode() != want {
					t.Fatalf("%s: got %v, want %v", name, fi.Mode(), want)
				}
			}
			fi, err := os.Stat(filepath.Join(target, "existing"))
			if err != nil {
				t.Fatalf("stat existing file: %v", err)
			}
			if fi.Mode() != 0o600 {
				t.Fatalf("existing file should keep its mode 0600, got %v", fi.Mode())
			}
		})
	}
}

func TestXattrsNotApplied(t *testing.T) {
	// Revision 2 file capability: effective, CAP_NET_RAW permitted.
	capability := "\x01\x00\x00\x02\x00\x20\x00\x00" + strings.Repeat("\x00", 12)
	records := map[string]string{
		"SCHILY.xattr.security.capability": capability,
		"SCHILY.xattr.user.n8n-test":       "x",
	}
	dir := tarDir("bin/")
	dir.hdr.PAXRecords = records
	file := tarFile("bin/ping", "x")
	file.hdr.Mode = 0o755
	file.hdr.PAXRecords = records
	body := buildTar(t, dir, file)

	_, target := newTarget(t)
	if _, err := unpackBoth(t, target, body, Policy{AllowSymlinks: true, MaxEntries: 10, KeepExecBit: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The OS may add attributes of its own, such as macOS's com.apple.provenance.
	for _, name := range []string{"bin", "bin/ping"} {
		for _, attr := range xattrNames(t, filepath.Join(target, name)) {
			if attr == "security.capability" || attr == "user.n8n-test" {
				t.Fatalf("%s: extended attribute %s from the archive was applied", name, attr)
			}
		}
	}
}

func xattrNames(t *testing.T, path string) []string {
	t.Helper()
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		t.Fatalf("list xattrs of %s: %v", path, err)
	}
	buf := make([]byte, size)
	size, err = unix.Listxattr(path, buf)
	if err != nil {
		t.Fatalf("list xattrs of %s: %v", path, err)
	}
	return strings.FieldsFunc(string(buf[:size]), func(char rune) bool { return char == 0 })
}

func TestMaxEntries(t *testing.T) {
	body := buildTar(t,
		tarEntry{hdr: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}},
		tarDir("a/"), tarDir("b/"), tarDir("c/"),
	)
	// An archive's own count is the smallest cap that accepts it.
	for maxEntries, want := range map[int]struct {
		err     error
		entries int
	}{3: {ErrTooLarge, 3}, 4: {nil, 4}, 0: {nil, 4}} {
		_, target := newTarget(t)
		stats, err := unpackBoth(t, target, body, Policy{MaxEntries: maxEntries})
		if !errors.Is(err, want.err) {
			t.Fatalf("MaxEntries %d: expected %v, got %v", maxEntries, want.err, err)
		}
		if stats.Entries != want.entries {
			t.Fatalf("MaxEntries %d: counted %d entries, want %d", maxEntries, stats.Entries, want.entries)
		}
	}
}

func TestMaxBytes(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "abc"), tarDir("d/"), tarFile("d/b.txt", "de"))
	for maxBytes, want := range map[int64]error{4: ErrTooLarge, 5: nil, 0: nil} {
		_, target := newTarget(t)
		if _, err := unpackBoth(t, target, body, Policy{MaxBytes: maxBytes}); !errors.Is(err, want) {
			t.Fatalf("MaxBytes %d: expected %v, got %v", maxBytes, want, err)
		}
	}
}

func TestMaxDecompressedBytes(t *testing.T) {
	plain := buildTar(t, tarFile("a.txt", "abc"), tarDir("d/"))
	size := int64(len(plain))
	for _, compress := range []bool{false, true} {
		body := plain
		if compress {
			body = gzipped(t, plain)
		}
		for maxDecompressed, want := range map[int64]error{size - 1: ErrTooLarge, size: nil, math.MaxInt64: nil, 0: nil} {
			_, target := newTarget(t)
			stats, err := unpackBoth(t, target, body, Policy{MaxDecompressedBytes: maxDecompressed})
			if !errors.Is(err, want) || want != nil && errors.Is(err, ErrInvalid) {
				t.Fatalf("gzip=%v MaxDecompressedBytes %d: expected %v, got %v", compress, maxDecompressed, want, err)
			}
			// The count of one run is the cap the next run passes at.
			if want == nil && stats.DecompressedBytes != size {
				t.Fatalf("gzip=%v MaxDecompressedBytes %d: counted %d bytes, want %d", compress, maxDecompressed, stats.DecompressedBytes, size)
			}
		}
	}

	_, target := newTarget(t)
	padded := gzipped(t, append(bytes.Clone(plain), make([]byte, 4096)...))
	if stats, err := unpackBoth(t, target, padded, Policy{}); err != nil || stats.DecompressedBytes != size+4096 {
		t.Fatalf("data after the end: counted %d bytes, %v; want %d", stats.DecompressedBytes, err, size+4096)
	}

	big := strings.Repeat("x", 512<<10)
	for name, body := range map[string][]byte{
		"data after the end": gzipped(t, append(bytes.Clone(plain), make([]byte, 16<<20)...)),
		"pax header":         gzipped(t, buildTar(t, tarEntry{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"comment": big}}})),
		"file content":       gzipped(t, buildTar(t, tarFile("a.txt", big))),
	} {
		_, target := newTarget(t)
		_, err := unpackBoth(t, target, body, Policy{MaxEntries: 1, MaxDecompressedBytes: 64 << 10})
		if !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected %v, got %v", name, ErrTooLarge, err)
		}
	}
}

func TestCappedReader(t *testing.T) {
	for limit, want := range map[int64]error{5: ErrTooLarge, 6: nil} {
		var read int64
		capped := &cappedReader{r: iotest.OneByteReader(strings.NewReader("abcdef")), max: limit, read: &read}
		got, err := io.ReadAll(capped)
		if !errors.Is(err, want) || int64(len(got)) != min(limit, 6) || read != int64(len(got)) {
			t.Fatalf("cap %d: got %q, counted %d, %v", limit, got, read, err)
		}
		if again, err := capped.Read(make([]byte, 1)); want != nil && (again != 0 || !errors.Is(err, ErrTooLarge)) {
			t.Fatalf("cap %d: the error is not sticky: %d, %v", limit, again, err)
		}
	}

	var read int64
	uncapped := &cappedReader{r: strings.NewReader("abcdef"), read: &read}
	if got, err := io.ReadAll(uncapped); err != nil || string(got) != "abcdef" || read != 6 {
		t.Fatalf("no cap: got %q, counted %d, %v", got, read, err)
	}
}

func TestMaxPathBytes(t *testing.T) {
	policy := Policy{AllowSymlinks: true, MaxEntries: 10, MaxPathBytes: 20}
	atLimit := strings.Repeat("d/", 9) + "ab"
	for name, tt := range map[string]struct {
		body []byte
		want error
	}{
		"a name at the limit":          {buildTar(t, tarFile(atLimit, "x")), nil},
		"a name past the limit":        {buildTar(t, tarFile(atLimit+"c", "x")), ErrInvalid},
		"a link target at the limit":   {buildTar(t, tarSymlink("link", atLimit)), nil},
		"a link target past the limit": {buildTar(t, tarSymlink("link", "/"+atLimit)), ErrInvalid},
		// A pax header carries it; unpacking would open every directory.
		"a name of 100,000 directories": {buildTar(t, tarDir(strings.Repeat("d/", 100_000))), ErrInvalid},
	} {
		_, target := newTarget(t)
		_, err := unpackBoth(t, target, tt.body, policy)
		if !errors.Is(err, tt.want) || tt.want != nil && !strings.Contains(err.Error(), "longer than 20 bytes") {
			t.Fatalf("%s: expected %v for the length, got %v", name, tt.want, err)
		}
		if entries, err := os.ReadDir(target); err != nil || tt.want != nil && len(entries) != 0 {
			t.Fatalf("%s: a refused entry was written: %v, %v", name, entries, err)
		}
	}

	_, target := newTarget(t)
	if _, err := unpackBoth(t, target, buildTar(t, tarFile(atLimit+"c", "x")), Policy{}); err != nil {
		t.Fatalf("without the cap: %v", err)
	}
}

func TestPolicyChecks(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "abc"))
	for name, p := range map[string]Policy{
		"MaxEntries":           {MaxEntries: -1},
		"MaxBytes":             {MaxBytes: -1},
		"MaxDecompressedBytes": {MaxDecompressedBytes: -1},
		"MaxPathBytes":         {MaxPathBytes: -1},
	} {
		_, target := newTarget(t)
		if _, err := unpackBoth(t, target, body, p); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("negative %s: expected %v, got %v", name, ErrTooLarge, err)
		}
		if _, err := os.Lstat(filepath.Join(target, "a.txt")); !os.IsNotExist(err) {
			t.Fatalf("negative %s: a.txt was written, stat err: %v", name, err)
		}
	}

	_, target := newTarget(t)
	_, err := unpackBoth(t, target, body, Policy{AllowSymlinks: true})
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected AllowSymlinks without MaxEntries to be refused, got %v", err)
	}
}

func TestMaxBytesRefusesExpandingGzip(t *testing.T) {
	body := gzipped(t, buildTar(t, tarFile("a.txt", "abc"), tarFile("bomb", strings.Repeat("\x00", 16<<20))))
	if len(body) > 64<<10 {
		t.Fatalf("test archive should be small, got %d bytes", len(body))
	}
	_, target := newTarget(t)

	stats, err := unpackBoth(t, target, body, Policy{MaxBytes: 1 << 20})
	var entryErr *EntryError
	if !errors.Is(err, ErrTooLarge) || !errors.As(err, &entryErr) || entryErr.Name != "bomb" {
		t.Fatalf("expected the expanding entry to be refused, got %v", err)
	}
	if stats.Entries != 1 || stats.Files != 1 || stats.Bytes != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "bomb")); !os.IsNotExist(statErr) {
		t.Fatalf("refused entry was written, stat err: %v", statErr)
	}
}

func TestFileSystemConflictsOnlyFailExtract(t *testing.T) {
	tests := map[string]struct {
		setup func(t *testing.T, base, target string)
		body  []byte
	}{
		"file then file under it": {body: buildTar(t, tarFile("x", "x"), tarFile("x/y", "y"))},
		"file then directory":     {body: buildTar(t, tarFile("x", "x"), tarDir("x/"))},
		"existing symlink out of the target": {
			setup: func(t *testing.T, base, target string) {
				if err := os.Symlink(base, filepath.Join(target, "link")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			body: buildTar(t, tarFile("link/outside.txt", "x")),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			base, target := newTarget(t)
			if tc.setup != nil {
				tc.setup(t, base, target)
			}
			root, err := os.OpenRoot(target)
			if err != nil {
				t.Fatalf("open root: %v", err)
			}
			defer root.Close()

			_, err = Extract(bytes.NewReader(tc.body), root, Policy{})
			var entryErr *EntryError
			if !errors.As(err, &entryErr) || errors.Is(err, ErrInvalid) {
				t.Fatalf("expected a write error, got %v", err)
			}
			if _, err := Validate(bytes.NewReader(tc.body), Policy{}); err != nil {
				t.Fatalf("Validate: unexpected error: %v", err)
			}
			if _, statErr := os.Lstat(filepath.Join(base, "outside.txt")); !os.IsNotExist(statErr) {
				t.Fatalf("an entry landed outside the target, stat err: %v", statErr)
			}
		})
	}
}

func TestEntryError(t *testing.T) {
	err := error(&EntryError{Name: "a b", Err: fmt.Errorf("%w: boom", ErrInvalid)})
	if got := err.Error(); got != `entry "a b": invalid archive: boom` {
		t.Fatalf("unexpected message: %s", got)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("EntryError should unwrap to its cause")
	}
}
