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
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

var (
	sharedKey = Key{Scope: volume.SharedScope, ID: "python", Kind: volume.KindExt4}
	tenantKey = Key{Scope: testTenant, ID: "deps", Kind: volume.KindTar}
)

func newTestLocal(t *testing.T) (*Local, string) {
	t.Helper()
	dir := t.TempDir()
	local, err := NewLocal(dir, "http://api.test:8080/")
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
	assertNoFiles(t, filepath.Join(dir, "volumes"))
}

func TestLocalPermissions(t *testing.T) {
	local, dir := newTestLocal(t)
	if _, err := local.Put(context.Background(), tenantKey, strings.NewReader("x"), PutOptions{MaxBytes: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	root := filepath.Join(dir, "volumes")
	for _, directory := range []string{root, filepath.Join(root, "tenants"), filepath.Join(root, "tenants", testTenant)} {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, %v; want 0700", directory, info.Mode().Perm(), err)
		}
	}
	if info, err := os.Stat(local.path(tenantKey)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("object mode %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestNewLocalTightensRootAndRemovesTempFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "volumes")
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "shared", tempPrefix+"123")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(dir, "http://api.test"); err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if info, _ := os.Stat(root); info.Mode().Perm() != 0o700 {
		t.Errorf("root mode %v, want 0700", info.Mode().Perm())
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp file still there: %v", err)
	}
}

func TestNewLocalNeedsTheDataDir(t *testing.T) {
	if _, err := NewLocal(filepath.Join(t.TempDir(), "missing"), "http://api.test"); err == nil {
		t.Fatal("NewLocal with a missing data dir succeeded")
	}
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
			assertNoFiles(t, filepath.Join(dir, "volumes"))
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
	assertNoFiles(t, filepath.Join(dir, "volumes"))
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

// Both writers pass the existence check before either commits, so only the
// link decides.
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
	_ = filepath.WalkDir(filepath.Join(dir, "volumes"), func(_ string, entry fs.DirEntry, _ error) error {
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
	if want := "http://api.test:8080" + LocalObjectPath + "tenants/" + testTenant + "/deps.tar"; parsed.Scheme+"://"+parsed.Host+parsed.Path != want {
		t.Errorf("URL %s, want it under %s", readURL, want)
	}
	if got, want := parsed.Query().Get("expires"), strconv.FormatInt(now.Add(MaxReadURLTTL).Unix(), 10); got != want {
		t.Errorf("expires = %s, want %s", got, want)
	}
	if _, err := local.ReadURL(context.Background(), tenantKey, 0); err == nil {
		t.Error("ReadURL with a zero ttl succeeded")
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

	other, err := NewLocal(t.TempDir(), "http://api.test:8080")
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
