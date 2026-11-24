package volumestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

var (
	sharedKey = Key{Scope: volume.SharedScope, ID: "python", Kind: volume.KindExt4}
	tenantKey = Key{Scope: testTenant, ID: "deps", Kind: volume.KindTar}
)

// newTestLocal returns a store with a random key and its directory.
func newTestLocal(t *testing.T) (*Local, string) {
	t.Helper()
	dir, _ := sharedStoreDirs(t)
	local, err := NewLocal(dir, "https://api.test:8443/", LocalOptions{})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return local, dir
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// assertNoFiles fails if anything but directories exists under root.
func assertNoFiles(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			t.Errorf("unexpected file %s", path)
		}
		return nil
	})
}

func TestLocalPutStatOpenDelete(t *testing.T) {
	local, dir := newTestLocal(t)
	ctx := context.Background()
	data := []byte("ext4 image bytes")

	for _, key := range []Key{sharedKey, tenantKey} {
		obj, err := local.Put(ctx, key, bytes.NewReader(data), PutOptions{MaxBytes: int64(len(data)), ExpectedSHA256: digest(data)})
		if err != nil {
			t.Fatalf("Put(%+v): %v", key, err)
		}
		want := Object{Size: int64(len(data)), SHA256: digest(data)}
		if obj != want {
			t.Fatalf("Put = %+v, want %+v", obj, want)
		}
		if got, err := local.Stat(ctx, key); err != nil || got != want {
			t.Fatalf("Stat = %+v, %v; want %+v", got, err, want)
		}
		rc, got, err := local.Open(ctx, key)
		if err != nil || got != want {
			t.Fatalf("Open = %+v, %v; want %+v", got, err, want)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(content, data) {
			t.Fatalf("Open content = %q, %v; want %q", content, err, data)
		}
		if err := local.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := local.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
		}
		if _, _, err := local.Open(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
		}
		if err := local.Delete(ctx, key); err != nil {
			t.Fatalf("second Delete = %v, want nil", err)
		}
	}
	assertNoFiles(t, dir)
}

func TestLocalPermissions(t *testing.T) {
	local, dir := newTestLocal(t)
	if _, err := local.Put(context.Background(), tenantKey, strings.NewReader("x"), PutOptions{MaxBytes: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, directory := range []string{dir, filepath.Join(dir, "tenants"), filepath.Join(dir, "tenants", testTenant)} {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, %v; want 0700", directory, info.Mode().Perm(), err)
		}
	}
	if info, err := os.Stat(local.path(tenantKey)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("object mode %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

// Another API instance on a shared directory may be writing a fresh temp
// file, so startup removes only those older than a day.
func TestNewLocalTightensRootAndRemovesStaleTempFiles(t *testing.T) {
	root, _ := sharedStoreDirs(t)
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "shared", tempPrefix+"123")
	fresh := filepath.Join(root, "shared", tempPrefix+"456")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleTempAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	young := time.Now().Add(-staleTempAge + time.Minute)
	if err := os.Chtimes(fresh, young, young); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(root, "https://api.test", LocalOptions{}); err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if info, _ := os.Stat(root); info.Mode().Perm() != 0o700 {
		t.Errorf("root mode %v, want 0700", info.Mode().Perm())
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp file still there: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file removed: %v", err)
	}
	// The hard link check leaves nothing behind.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "shared" {
		t.Errorf("root holds %v, want only shared", entries)
	}
}

func TestNewLocalNeedsTheParentDir(t *testing.T) {
	if _, err := NewLocal(filepath.Join(t.TempDir(), "missing", "volumes"), "https://api.test", LocalOptions{}); err == nil {
		t.Fatal("NewLocal with a missing parent dir succeeded")
	}
}

func TestCheckHardLinksRefusesAFileSystemWithout(t *testing.T) {
	dir := t.TempDir()
	noLinks := func(string, string) error { return syscall.EOPNOTSUPP }
	err := checkHardLinks(dir, noLinks)
	if err == nil || !strings.Contains(err.Error(), "hard links") || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("checkHardLinks = %v, want an error about hard links that wraps EOPNOTSUPP", err)
	}
	// A copy is not a link either.
	copyFile := func(oldname, newname string) error {
		content, err := os.ReadFile(oldname)
		if err != nil {
			return err
		}
		return os.WriteFile(newname, content, 0o600)
	}
	if err := checkHardLinks(dir, copyFile); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("checkHardLinks with a copy = %v, want an error about hard links", err)
	}
	// Over NFS a link can fail though it was made.
	linkedButFailed := func(oldname, newname string) error {
		if err := os.Link(oldname, newname); err != nil {
			return err
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	}
	if err := checkHardLinks(dir, linkedButFailed); err != nil {
		t.Fatalf("checkHardLinks with a link that reports EEXIST = %v, want nil", err)
	}
	assertNoFiles(t, dir)
}

func TestLocalPutErrorsLeaveNothingVisible(t *testing.T) {
	data := []byte("0123456789")
	cases := []struct {
		name   string
		opts   PutOptions
		reader io.Reader
		want   error
	}{
		{"too large", PutOptions{MaxBytes: 9}, bytes.NewReader(data), ErrTooLarge},
		{"digest mismatch", PutOptions{MaxBytes: 10, ExpectedSHA256: digest([]byte("other"))}, bytes.NewReader(data), ErrDigestMismatch},
		{"reader error", PutOptions{MaxBytes: 100}, io.MultiReader(bytes.NewReader(data), iotestErrReader{}), errRead},
		{"too large with an error in the same read", PutOptions{MaxBytes: 9}, readerFunc(func(buf []byte) (int, error) { return copy(buf, data), errRead }), ErrTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			local, dir := newTestLocal(t)
			if _, err := local.Put(context.Background(), sharedKey, testCase.reader, testCase.opts); !errors.Is(err, testCase.want) {
				t.Fatalf("Put = %v, want %v", err, testCase.want)
			}
			if _, err := local.Stat(context.Background(), sharedKey); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Stat after failed Put = %v, want ErrNotFound", err)
			}
			assertNoFiles(t, dir)
			// The key is still free.
			if _, err := local.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10}); err != nil {
				t.Fatalf("Put after failed Put: %v", err)
			}
		})
	}
}

var errRead = errors.New("read failed")

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errRead }

func TestLocalPutStopsWhenContextEnds(t *testing.T) {
	local, dir := newTestLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := local.Put(ctx, sharedKey, strings.NewReader("x"), PutOptions{MaxBytes: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	assertNoFiles(t, dir)
}

func TestLocalPutRejectsBadArguments(t *testing.T) {
	local, _ := newTestLocal(t)
	ctx := context.Background()
	if _, err := local.Put(ctx, Key{Scope: "../x", ID: "a", Kind: volume.KindTar}, strings.NewReader("x"), PutOptions{MaxBytes: 1}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Put with an invalid key = %v, want ErrInvalidKey", err)
	}
	if _, err := local.Put(ctx, sharedKey, strings.NewReader("x"), PutOptions{}); err == nil {
		t.Error("Put without MaxBytes succeeded")
	}
	for _, expected := range []string{"abc", strings.ToUpper(digest([]byte("x")))} {
		if _, err := local.Put(ctx, sharedKey, strings.NewReader("x"), PutOptions{MaxBytes: 1, ExpectedSHA256: expected}); err == nil {
			t.Errorf("Put with ExpectedSHA256 %q succeeded", expected)
		}
	}
	for name, call := range map[string]func(Key) error{
		"Stat":    func(key Key) error { _, err := local.Stat(ctx, key); return err },
		"Open":    func(key Key) error { _, _, err := local.Open(ctx, key); return err },
		"Delete":  func(key Key) error { return local.Delete(ctx, key) },
		"ReadURL": func(key Key) error { _, err := local.ReadURL(ctx, key, time.Minute); return err },
	} {
		if err := call(Key{Scope: volume.SharedScope, ID: "../../api.db", Kind: volume.KindTar}); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s with an invalid key = %v, want ErrInvalidKey", name, err)
		}
	}
}

func TestLocalPutNeverOverwrites(t *testing.T) {
	local, _ := newTestLocal(t)
	ctx := context.Background()
	if _, err := local.Put(ctx, sharedKey, strings.NewReader("first"), PutOptions{MaxBytes: 10}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := local.Put(ctx, sharedKey, strings.NewReader("second"), PutOptions{MaxBytes: 10}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}
	if obj, _ := local.Stat(ctx, sharedKey); obj.SHA256 != digest([]byte("first")) {
		t.Fatalf("object was replaced: %+v", obj)
	}
}

// Concurrent Puts of one key: the link lets only one of them commit.
func TestLocalConcurrentPutsCommitOnce(t *testing.T) {
	local, dir := newTestLocal(t)
	ctx := context.Background()
	const writers = 8
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for index := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader := io.MultiReader(gateReader{release}, strings.NewReader(strconv.Itoa(index)))
			_, errs[index] = local.Put(ctx, sharedKey, reader, PutOptions{MaxBytes: 10})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	var ok int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrExists):
			t.Errorf("Put = %v, want nil or ErrExists", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d Puts succeeded, want 1", ok)
	}
	rc, obj, err := local.Open(ctx, sharedKey)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(rc)
	rc.Close()
	if obj.SHA256 != digest(content) {
		t.Fatalf("stored digest %s does not match stored content %q", obj.SHA256, content)
	}
	var files int
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, _ error) error {
		if !entry.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Fatalf("%d files left, want 1", files)
	}
}

// gateReader blocks its first Read until release is closed, then reads as
// empty.
type gateReader struct{ release chan struct{} }

func (g gateReader) Read([]byte) (int, error) {
	<-g.release
	return 0, io.EOF
}

func TestLocalRejectsCorruptFiles(t *testing.T) {
	local, _ := newTestLocal(t)
	ctx := context.Background()
	if _, err := local.Put(ctx, sharedKey, strings.NewReader("data"), PutOptions{MaxBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(local.path(sharedKey), int64(trailerLen)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Stat(ctx, sharedKey); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat of a truncated file = %v, want a corruption error", err)
	}
}

func TestLocalReadURLCapsTTL(t *testing.T) {
	local, _ := newTestLocal(t)
	now := time.Unix(1_800_000_000, 0)
	local.now = func() time.Time { return now }
	readURL, err := local.ReadURL(context.Background(), tenantKey, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(readURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.test:8443" + LocalObjectPath + "tenants/" + testTenant + "/deps.tar"; parsed.Scheme+"://"+parsed.Host+parsed.Path != want {
		t.Errorf("URL %s, want it under %s", readURL, want)
	}
	if got, want := parsed.Query().Get("expires"), strconv.FormatInt(now.Add(MaxReadURLTTL).Unix(), 10); got != want {
		t.Errorf("expires = %s, want %s", got, want)
	}
	if _, err := local.ReadURL(context.Background(), tenantKey, 0); err == nil {
		t.Error("ReadURL with a zero ttl succeeded")
	}

	// Expiry is in whole seconds: a short ttl must not round down into the past.
	now = time.Unix(1_800_000_000, 500_000_000)
	short, err := local.ReadURL(context.Background(), tenantKey, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.VerifyRequest(requestFor(t, http.MethodGet, short)); err != nil {
		t.Errorf("a fresh URL with a short ttl: %v", err)
	}
}

// requestFor turns a URL from ReadURL into the request the API would see.
func requestFor(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(method, parsed.RequestURI(), nil)
}

func TestLocalVerifyRequest(t *testing.T) {
	local, _ := newTestLocal(t)
	now := time.Unix(1_800_000_000, 0)
	local.now = func() time.Time { return now }
	signed, err := local.ReadURL(context.Background(), tenantKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(signed)
	query := parsed.Query()
	exp, sig := query.Get("expires"), query.Get("sig")
	otherTenant := strings.Replace(parsed.Path, testTenant, "1b6f3a52-7c1e-4d2a-9f3b-5e8d1c2a4b6f", 1)

	if key, err := local.VerifyRequest(requestFor(t, http.MethodGet, signed)); err != nil || key != tenantKey {
		t.Fatalf("VerifyRequest = %+v, %v; want %+v", key, err, tenantKey)
	}

	other, err := NewLocal(t.TempDir(), "https://api.test:8443", LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VerifyRequest(requestFor(t, http.MethodGet, signed)); !errors.Is(err, errBadSignature) {
		t.Errorf("another process accepted the URL: %v", err)
	}

	bad := map[string]string{
		"tampered path":       otherTenant + "?expires=" + exp + "&sig=" + sig,
		"tampered id":         strings.Replace(parsed.Path, "deps", "dep", 1) + "?expires=" + exp + "&sig=" + sig,
		"tampered kind":       strings.Replace(parsed.Path, ".tar", ".ext4", 1) + "?expires=" + exp + "&sig=" + sig,
		"tampered expiry":     parsed.Path + "?expires=" + strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + "&sig=" + sig,
		"padded expiry":       parsed.Path + "?expires=0" + exp + "&sig=" + sig,
		"tampered signature":  parsed.Path + "?expires=" + exp + "&sig=" + strings.Repeat("0", len(sig)),
		"short signature":     parsed.Path + "?expires=" + exp + "&sig=" + sig[:len(sig)-2],
		"non-hex signature":   parsed.Path + "?expires=" + exp + "&sig=" + strings.Repeat("z", len(sig)),
		"no signature":        parsed.Path + "?expires=" + exp,
		"no expiry":           parsed.Path + "?sig=" + sig,
		"two expiries":        parsed.Path + "?expires=" + exp + "&expires=" + exp + "&sig=" + sig,
		"two signatures":      parsed.Path + "?expires=" + exp + "&sig=" + sig + "&sig=" + sig,
		"traversal":           LocalObjectPath + "shared/../tenants/" + testTenant + "/deps.tar?expires=" + exp + "&sig=" + sig,
		"other route":         "/sandboxes?expires=" + exp + "&sig=" + sig,
		"malformed query":     parsed.Path + "?expires=" + exp + "&sig=" + sig + "&x=%zz",
		"semicolon separator": parsed.Path + "?expires=" + exp + ";sig=" + sig,
	}
	for name, target := range bad {
		if _, err := local.VerifyRequest(httptest.NewRequest(http.MethodGet, target, nil)); !errors.Is(err, errBadSignature) {
			t.Errorf("%s: VerifyRequest = %v, want errBadSignature", name, err)
		}
	}
	for _, method := range []string{http.MethodHead, http.MethodPut, http.MethodDelete} {
		if _, err := local.VerifyRequest(requestFor(t, method, signed)); !errors.Is(err, errBadSignature) {
			t.Errorf("%s: VerifyRequest = %v, want errBadSignature", method, err)
		}
	}

	now = now.Add(time.Minute)
	if _, err := local.VerifyRequest(requestFor(t, http.MethodGet, signed)); !errors.Is(err, errExpiredSignature) {
		t.Errorf("at expiry: VerifyRequest = %v, want errExpiredSignature", err)
	}
	now = now.Add(time.Hour)
	if _, err := local.VerifyRequest(requestFor(t, http.MethodGet, signed)); !errors.Is(err, errExpiredSignature) {
		t.Errorf("past expiry: VerifyRequest = %v, want errExpiredSignature", err)
	}
}

func TestLocalServeObject(t *testing.T) {
	local, _ := newTestLocal(t)
	data := []byte("0123456789")
	if _, err := local.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	if err := local.ServeObject(rr, httptest.NewRequest(http.MethodGet, "/", nil), sharedKey); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("got %d %q, want 200 %q", rr.Code, rr.Body.Bytes(), data)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Range", "bytes=8-")
	rr = httptest.NewRecorder()
	if err := local.ServeObject(rr, req, sharedKey); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "89" {
		t.Fatalf("range: got %d %q, want 206 \"89\"", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	if err := local.ServeObject(rr, httptest.NewRequest(http.MethodGet, "/", nil), tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: ServeObject = %v, want ErrNotFound", err)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("missing object: wrote %q", rr.Body.String())
	}
}

var (
	testSigningKey  = bytes.Repeat([]byte{0x5a}, MinSigningKeyBytes)
	otherSigningKey = bytes.Repeat([]byte{0xa5}, MinSigningKeyBytes)
)

// sharedStoreDirs returns one store directory under two paths, for two
// stores that stand for two API instances. With SANDBOX_TEST_VOLUME_LOCAL_DIRS,
// two mounts of one NFS export separated by a comma (mounted with
// nosharecache, so each keeps caches of its own), the paths reach it through
// those mounts; otherwise both are the same local directory.
func sharedStoreDirs(t *testing.T) (string, string) {
	t.Helper()
	mounts := os.Getenv("SANDBOX_TEST_VOLUME_LOCAL_DIRS")
	if mounts == "" {
		dir := filepath.Join(t.TempDir(), "volumes")
		return dir, dir
	}
	first, second, ok := strings.Cut(mounts, ",")
	if !ok {
		t.Fatalf("SANDBOX_TEST_VOLUME_LOCAL_DIRS must be two paths separated by a comma, got %q", mounts)
	}
	parent, err := os.MkdirTemp(first, "test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	name := filepath.Base(parent)
	return filepath.Join(first, name, "volumes"), filepath.Join(second, name, "volumes")
}

func newSharedTestLocal(t *testing.T, dir string, opts LocalOptions) *Local {
	t.Helper()
	local, err := NewLocal(dir, "https://api.test:8443", opts)
	if err != nil {
		t.Fatalf("NewLocal(%s): %v", dir, err)
	}
	return local
}

// serve fetches readURL from local the way the volume-objects listener does.
func serve(t *testing.T, local *Local, readURL string) (int, string, error) {
	t.Helper()
	req := requestFor(t, http.MethodGet, readURL)
	key, err := local.VerifyRequest(req)
	if err != nil {
		return 0, "", err
	}
	recorder := httptest.NewRecorder()
	if err := local.ServeObject(recorder, req, key); err != nil {
		return 0, "", err
	}
	return recorder.Code, recorder.Body.String(), nil
}

// Several API instances share one directory and one key: a URL any of them
// signs, every one of them verifies and serves.
func TestLocalSharedDirectoryAndKey(t *testing.T) {
	firstDir, secondDir := sharedStoreDirs(t)
	first := newSharedTestLocal(t, firstDir, LocalOptions{SigningKey: testSigningKey})
	second := newSharedTestLocal(t, secondDir, LocalOptions{SigningKey: testSigningKey})
	ctx := context.Background()
	for _, key := range []Key{sharedKey, tenantKey} {
		if _, err := first.Put(ctx, key, strings.NewReader("image bytes"), PutOptions{MaxBytes: 100}); err != nil {
			t.Fatalf("Put(%+v): %v", key, err)
		}
		for _, signer := range []*Local{first, second} {
			readURL, err := signer.ReadURL(ctx, key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			for name, server := range map[string]*Local{"first": first, "second": second} {
				status, body, err := serve(t, server, readURL)
				if err != nil || status != http.StatusOK || body != "image bytes" {
					t.Errorf("%+v from the %s store: %d %q %v, want 200 \"image bytes\"", key, name, status, body, err)
				}
			}
		}
	}

	readURL, err := first.ReadURL(ctx, sharedKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]LocalOptions{
		"another key":  {SigningKey: otherSigningKey},
		"a random key": {},
	} {
		stranger := newSharedTestLocal(t, secondDir, opts)
		if _, _, err := serve(t, stranger, readURL); !errors.Is(err, errBadSignature) {
			t.Errorf("a store with %s: %v, want errBadSignature", name, err)
		}
	}
}

// During a rotation the previous key verifies the URLs it signed, while new
// URLs carry the new key only.
func TestLocalPreviousSigningKey(t *testing.T) {
	firstDir, secondDir := sharedStoreDirs(t)
	before := newSharedTestLocal(t, firstDir, LocalOptions{SigningKey: testSigningKey})
	rotated := newSharedTestLocal(t, secondDir, LocalOptions{SigningKey: otherSigningKey, PreviousSigningKey: testSigningKey})
	after := newSharedTestLocal(t, secondDir, LocalOptions{SigningKey: otherSigningKey})
	ctx := context.Background()
	if _, err := before.Put(ctx, sharedKey, strings.NewReader("image bytes"), PutOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}

	oldURL, err := before.ReadURL(ctx, sharedKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if status, body, err := serve(t, rotated, oldURL); err != nil || status != http.StatusOK || body != "image bytes" {
		t.Errorf("an old URL during the rotation: %d %q %v, want 200", status, body, err)
	}
	if _, _, err := serve(t, after, oldURL); !errors.Is(err, errBadSignature) {
		t.Errorf("an old URL after the rotation: %v, want errBadSignature", err)
	}

	newURL, err := rotated.ReadURL(ctx, sharedKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := serve(t, after, newURL); err != nil {
		t.Errorf("a new URL after the rotation: %v", err)
	}
	if _, _, err := serve(t, before, newURL); !errors.Is(err, errBadSignature) {
		t.Errorf("a new URL with the old key alone: %v, want errBadSignature; it must be signed with the new key", err)
	}
}

func TestNewLocalRefusesBadSigningKeys(t *testing.T) {
	short := bytes.Repeat([]byte{0x33}, MinSigningKeyBytes-1)
	for name, opts := range map[string]LocalOptions{
		"a short key":                  {SigningKey: short},
		"a short previous key":         {SigningKey: testSigningKey, PreviousSigningKey: short},
		"a previous key without a key": {PreviousSigningKey: testSigningKey},
	} {
		dir := filepath.Join(t.TempDir(), "volumes")
		_, err := NewLocal(dir, "https://api.test", opts)
		if err == nil {
			t.Errorf("%s: NewLocal succeeded", name)
			continue
		}
		for _, key := range [][]byte{short, testSigningKey} {
			if strings.Contains(err.Error(), string(key)) || strings.Contains(err.Error(), hex.EncodeToString(key)) {
				t.Errorf("%s: the error shows a key: %v", name, err)
			}
		}
		// Refused before it touches the directory.
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: NewLocal created %s: %v", name, dir, err)
		}
	}
}

// Two API instances put the same key at once: one commits, the other gets
// ErrExists, and the object is one writer's, whole.
func TestLocalConcurrentPutsFromTwoStoresCommitOnce(t *testing.T) {
	firstDir, secondDir := sharedStoreDirs(t)
	stores := []*Local{
		newSharedTestLocal(t, firstDir, LocalOptions{SigningKey: testSigningKey}),
		newSharedTestLocal(t, secondDir, LocalOptions{SigningKey: testSigningKey}),
	}
	ctx := context.Background()
	const writers = 8
	const size = 1 << 20
	payloads := make([][]byte, writers)
	for index := range payloads {
		payloads[index] = bytes.Repeat([]byte{byte('a' + index)}, size)
	}
	for round := range 3 {
		key := Key{Scope: volume.SharedScope, ID: "race-" + strconv.Itoa(round), Kind: volume.KindTar}
		release := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for index := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reader := io.MultiReader(gateReader{release}, bytes.NewReader(payloads[index]))
				_, errs[index] = stores[index%2].Put(ctx, key, reader, PutOptions{MaxBytes: size})
			}()
		}
		time.Sleep(50 * time.Millisecond)
		close(release)
		wg.Wait()

		winner := -1
		for index, err := range errs {
			switch {
			case err == nil && winner >= 0:
				t.Fatalf("round %d: writers %d and %d both committed", round, winner, index)
			case err == nil:
				winner = index
			case !errors.Is(err, ErrExists):
				t.Errorf("round %d: Put = %v, want nil or ErrExists", round, err)
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: no Put committed", round)
		}
		for name, store := range map[string]*Local{"first": stores[0], "second": stores[1]} {
			rc, obj, err := store.Open(ctx, key)
			if err != nil {
				t.Fatalf("round %d: Open from the %s store: %v", round, name, err)
			}
			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || !bytes.Equal(content, payloads[winner]) || obj.SHA256 != digest(payloads[winner]) {
				t.Fatalf("round %d: the %s store reads %d bytes, digest %s, %v; want writer %d's object, whole", round, name, len(content), obj.SHA256, err, winner)
			}
		}
	}
	var temps []string
	_ = filepath.WalkDir(firstDir, func(path string, entry fs.DirEntry, _ error) error {
		if entry != nil && strings.HasPrefix(entry.Name(), tempPrefix) {
			temps = append(temps, path)
		}
		return nil
	})
	if len(temps) > 0 {
		t.Errorf("temp files left: %v", temps)
	}
}

// An instance that has seen an object can store it again right after another
// instance deletes it, although over NFS its cache still holds the name.
func TestLocalPutAfterAnotherStoreDeletes(t *testing.T) {
	firstDir, secondDir := sharedStoreDirs(t)
	first := newSharedTestLocal(t, firstDir, LocalOptions{SigningKey: testSigningKey})
	second := newSharedTestLocal(t, secondDir, LocalOptions{SigningKey: testSigningKey})
	ctx := context.Background()
	if _, err := first.Put(ctx, sharedKey, strings.NewReader("old"), PutOptions{MaxBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Stat(ctx, sharedKey); err != nil {
		t.Fatalf("Stat from the second store: %v", err)
	}
	if err := first.Delete(ctx, sharedKey); err != nil {
		t.Fatal(err)
	}
	obj, err := second.Put(ctx, sharedKey, strings.NewReader("new"), PutOptions{MaxBytes: 10})
	if err != nil {
		t.Fatalf("Put from the second store after the first deleted the object: %v", err)
	}
	if got, err := first.Stat(ctx, sharedKey); err != nil || got != obj {
		t.Fatalf("Stat from the first store = %+v, %v; want %+v", got, err, obj)
	}
}

// Over NFS a link that was made can report an error, such as EEXIST for its
// own retried request. The object is committed all the same.
func TestLocalPutCommitsWhenTheLinkReportsAnErrorButWasMade(t *testing.T) {
	local, dir := newTestLocal(t)
	local.link = func(oldname, newname string) error {
		if err := os.Link(oldname, newname); err != nil {
			return err
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	}
	ctx := context.Background()
	obj, err := local.Put(ctx, sharedKey, strings.NewReader("data"), PutOptions{MaxBytes: 10})
	if err != nil {
		t.Fatalf("Put = %v, want the object committed", err)
	}
	if got, err := local.Stat(ctx, sharedKey); err != nil || got != obj {
		t.Fatalf("Stat = %+v, %v; want %+v", got, err, obj)
	}
	var files int
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, _ error) error {
		if !entry.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Fatalf("%d files left, want 1", files)
	}
	// A link that failed and was not made stays an error.
	local.link = func(string, string) error { return syscall.EIO }
	if _, err := local.Put(ctx, tenantKey, strings.NewReader("data"), PutOptions{MaxBytes: 10}); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Put with a failed link = %v, want EIO", err)
	}
	if _, err := local.Stat(ctx, tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after a failed link = %v, want ErrNotFound", err)
	}
}

func TestCachesMissingNames(t *testing.T) {
	const mountinfo = `22 1 0:21 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u
40 22 0:45 / /var/lib/n8n-sandbox-volumes rw,relatime shared:12 - nfs4 fs.example:/ rw,vers=4.1,rsize=1048576,hard,proto=tcp,sec=sys
41 22 0:46 / /mnt/positive rw,relatime - nfs4 fs.example:/ rw,vers=4.2,lookupcache=pos,sec=sys
42 22 0:47 / /mnt/none rw,relatime - nfs fs.example:/export rw,vers=3,lookupcache=none
43 40 8:1 / /var/lib/n8n-sandbox-volumes/local rw,relatime - ext4 /dev/sda1 rw
44 22 0:48 / /mnt/with\040space rw,relatime - nfs fs.example:/export rw,vers=3
45 22 0:49 / /mnt/stacked rw,relatime - nfs4 fs.example:/ rw,vers=4.2
46 22 0:50 / /mnt/stacked rw,relatime - nfs4 fs.example:/ rw,vers=4.2,lookupcache=positive
47 22 0:51 / /mnt/ceph rw,relatime - ceph 10.0.0.1:/ rw,name=api
`
	for dir, want := range map[string]bool{
		"/var/lib/n8n-sandbox-volumes/volumes":       true,
		"/var/lib/n8n-sandbox-volumes":               true,
		"/var/lib/n8n-sandbox-volumes-other":         false,
		"/var/lib/n8n-sandbox-volumes/local/volumes": false,
		"/mnt/positive/volumes":                      false,
		"/mnt/none/volumes":                          false,
		"/mnt/with space/volumes":                    true,
		"/mnt/stacked/volumes":                       false,
		"/mnt/ceph/volumes":                          false,
		"/var/lib/n8n-sandbox-api/volumes":           false,
	} {
		if got := cachesMissingNames(mountinfo, dir); got != want {
			t.Errorf("cachesMissingNames(%s) = %v, want %v", dir, got, want)
		}
	}
	if cachesMissingNames("garbage\n\n1 2 3", "/var/lib/n8n-sandbox-volumes") {
		t.Error("unreadable mountinfo reported an NFS mount")
	}
}
