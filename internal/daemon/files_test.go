package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestHandleFileCopyRejectsNestedDestination(t *testing.T) {
	base := tempBaseDir(t)
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	err := HandleFileCopy(srcDir, filepath.Join(srcDir, "child"), true, false)
	if err == nil {
		t.Fatal("expected error when copying directory into its own subtree")
	}
	if !strings.Contains(err.Error(), "destination must not be inside source directory") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(srcDir, "child")); !os.IsNotExist(statErr) {
		t.Fatalf("destination directory should not be created, stat err: %v", statErr)
	}
}

func TestHandleFileMoveRejectsNestedDestination(t *testing.T) {
	base := tempBaseDir(t)
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	err := HandleFileMove(srcDir, filepath.Join(srcDir, "child"), false)
	if err == nil {
		t.Fatal("expected error when moving directory into its own subtree")
	}
	if !strings.Contains(err.Error(), "destination must not be inside source directory") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(srcDir, "a.txt")); statErr != nil {
		t.Fatalf("source should remain untouched, stat err: %v", statErr)
	}
}

type tarEntry struct {
	hdr  tar.Header
	body string
}

func tarFile(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: body}
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
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
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
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

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestHandleFileArchiveWritesFiles(t *testing.T) {
	for _, compress := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%v", compress), func(t *testing.T) {
			target := filepath.Join(tempBaseDir(t), "new", "target")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir target: %v", err)
			}
			if err := os.WriteFile(filepath.Join(target, "x.txt"), []byte("old content"), 0o600); err != nil {
				t.Fatalf("write existing file: %v", err)
			}
			// What the daemon's 0755 turns into under this process's umask.
			reference := filepath.Join(t.TempDir(), "reference")
			if err := os.Mkdir(reference, 0o755); err != nil {
				t.Fatalf("mkdir reference: %v", err)
			}
			refInfo, err := os.Stat(reference)
			if err != nil {
				t.Fatalf("stat reference: %v", err)
			}

			setuid := tarFile("deep/nested/y.txt", "yy")
			setuid.hdr.Mode = 0o4755
			// "./" and "./"-prefixed names are what `tar -C dir .` writes.
			body := buildTar(t,
				tarEntry{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
				tarEntry{hdr: tar.Header{Name: "./empty/", Typeflag: tar.TypeDir, Mode: 0o700}},
				tarFile("./x.txt", "new"),
				setuid,
			)
			if compress {
				body = gzipped(t, body)
			}

			res, err := HandleFileArchive(target, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res != (ArchiveResult{Files: 2, Bytes: 5}) {
				t.Fatalf("unexpected result: %+v", res)
			}
			if got := readString(t, filepath.Join(target, "x.txt")); got != "new" {
				t.Fatalf("existing file not overwritten: %q", got)
			}
			if fi, err := os.Stat(filepath.Join(target, "x.txt")); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("existing file should keep its mode 0600, got %v (err %v)", fi.Mode(), err)
			}
			if got := readString(t, filepath.Join(target, "deep", "nested", "y.txt")); got != "yy" {
				t.Fatalf("nested file: %q", got)
			}
			fi, err := os.Stat(filepath.Join(target, "deep", "nested", "y.txt"))
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
			if fi.Mode().Perm() != refInfo.Mode().Perm() {
				t.Fatalf("archive directory mode was applied: got %v, want %v", fi.Mode().Perm(), refInfo.Mode().Perm())
			}
		})
	}
}

func TestHandleFileArchiveCreatesTargetDirectory(t *testing.T) {
	target := filepath.Join(tempBaseDir(t), "a", "b")

	if _, err := HandleFileArchive(target, bytes.NewReader(buildTar(t, tarFile("f.txt", "f")))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := readString(t, filepath.Join(target, "f.txt")); got != "f" {
		t.Fatalf("file: %q", got)
	}
}

func TestHandleFileArchiveKeepsEntriesBeforeFailure(t *testing.T) {
	target := tempBaseDir(t)
	body := buildTar(t,
		tarFile("first.txt", "1"),
		tarEntry{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "first.txt"}},
		tarFile("after.txt", "2"),
	)

	if _, err := HandleFileArchive(target, bytes.NewReader(body)); err == nil {
		t.Fatal("expected the symlink to be refused")
	}
	if got := readString(t, filepath.Join(target, "first.txt")); got != "1" {
		t.Fatalf("entry before the failure: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(target, "after.txt")); !os.IsNotExist(err) {
		t.Fatalf("entry after the failure was written, stat err: %v", err)
	}
}

func TestHandleFileArchiveRefusals(t *testing.T) {
	truncated := buildTar(t, tarFile("big.txt", strings.Repeat("x", 100)))[:512+10]

	tests := []struct {
		name string
		// body builds the archive; base is the parent of the target directory,
		// where nothing may be written.
		body func(t *testing.T, base string) []byte
		// setup prepares the target directory before the unpack.
		setup func(t *testing.T, base, target string)
	}{
		{
			name: "absolute path",
			body: func(t *testing.T, base string) []byte {
				return buildTar(t, tarFile(filepath.Join(base, "outside.txt"), "x"))
			},
		},
		{
			name: "parent directory",
			body: func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("../outside.txt", "x")) },
		},
		{
			name: "parent directory that stays inside",
			body: func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("a/../outside.txt", "x")) },
		},
		{
			name: "symlink",
			body: func(t *testing.T, base string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeSymlink, Linkname: base}})
			},
		},
		{
			name: "hard link",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarFile("a.txt", "a"), tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeLink, Linkname: "a.txt"}})
			},
		},
		{
			name: "character device",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}})
			},
		},
		{
			name: "block device",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeBlock, Devmajor: 8}})
			},
		},
		{
			name: "fifo",
			body: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "outside.txt", Typeflag: tar.TypeFifo}})
			},
		},
		{
			name: "existing symlink out of the target",
			setup: func(t *testing.T, base, target string) {
				if err := os.Symlink(base, filepath.Join(target, "link")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			body: func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("link/outside.txt", "x")) },
		},
		{
			name: "invalid tar header",
			body: func(t *testing.T, _ string) []byte { return bytes.Repeat([]byte("x"), 512) },
		},
		{
			name: "truncated file content",
			body: func(t *testing.T, _ string) []byte { return truncated },
		},
		{
			name: "invalid gzip",
			body: func(t *testing.T, _ string) []byte {
				return append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("x"), 20)...)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := tempBaseDir(t)
			target := filepath.Join(base, "target")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir target: %v", err)
			}
			if tc.setup != nil {
				tc.setup(t, base, target)
			}

			_, err := HandleFileArchive(target, bytes.NewReader(tc.body(t, base)))
			if err == nil {
				t.Fatal("expected the archive to be refused")
			}
			if code := fileOpStatusCode(err); code != http.StatusBadRequest {
				t.Fatalf("expected %d, got %d: %v", http.StatusBadRequest, code, err)
			}
			if _, statErr := os.Lstat(filepath.Join(base, "outside.txt")); !os.IsNotExist(statErr) {
				t.Fatalf("an entry landed outside the target, stat err: %v", statErr)
			}
			if _, statErr := os.Lstat(filepath.Join(target, "outside.txt")); !os.IsNotExist(statErr) {
				t.Fatalf("a refused entry was written, stat err: %v", statErr)
			}
		})
	}
}

// The tar ends before the gzip stream does, so a broken gzip footer only shows
// when the stream is read to its end.
func TestHandleFileArchiveChecksGzipFooter(t *testing.T) {
	valid := gzipped(t, buildTar(t, tarFile("a.txt", "a")))
	badChecksum := bytes.Clone(valid)
	badChecksum[len(badChecksum)-8] ^= 0xff

	for name, body := range map[string][]byte{
		"bad checksum":   badChecksum,
		"missing footer": valid[:len(valid)-8],
	} {
		t.Run(name, func(t *testing.T) {
			_, err := HandleFileArchive(tempBaseDir(t), bytes.NewReader(body))
			if err == nil {
				t.Fatal("expected the broken gzip stream to be refused")
			}
			if code := fileOpStatusCode(err); code != http.StatusBadRequest {
				t.Fatalf("expected %d, got %d: %v", http.StatusBadRequest, code, err)
			}
		})
	}
}

// A pax sparse file reaches the caller as a regular file, so the type check
// alone would let a tiny body write gigabytes of zeros.
func TestHandleFileArchiveRefusesSparseFiles(t *testing.T) {
	target := tempBaseDir(t)

	_, err := HandleFileArchive(target, bytes.NewReader(sparseTar(t, "sparse.bin", 1<<30)))
	if err == nil || !strings.Contains(err.Error(), "sparse files are not allowed") {
		t.Fatalf("expected the sparse file to be refused, got %v", err)
	}
	if code := fileOpStatusCode(err); code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d", http.StatusBadRequest, code)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "sparse.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("sparse file was written, stat err: %v", statErr)
	}
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

func tempBaseDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "daemon-files-test-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// Resolve symlinks so the test path is stable on macOS (/var vs /private/var).
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	return resolvedBase
}
