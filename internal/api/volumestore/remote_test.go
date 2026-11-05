package volumestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

// fakeHelper serves the helper protocol from memory. Unless strict, it
// ignores max_bytes and sha256 and commits any complete body, so tests can
// show what the API enforces on its own.
type fakeHelper struct {
	t      *testing.T
	strict bool

	mu        sync.Mutex
	objects   map[string][]byte
	commits   int
	requests  int
	received  int  // most body bytes seen by one PUT
	lieDigest bool // reports a digest other than the one stored
	status    int  // answers every request with this status when set
	readURL   string
	readReqs  []map[string]any

	earlyCreated    bool          // stores the first 3 bytes and answers 201 before the body ends
	failAfterCommit bool          // stores the object, then answers 500
	hang            chan struct{} // holds every request until closed
}

func newFakeHelper(t *testing.T) (*fakeHelper, *Remote) {
	t.Helper()
	// Unix socket paths are short; t.TempDir is too long on macOS.
	dir, err := os.MkdirTemp("", "vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	helper := &fakeHelper{t: t, objects: map[string][]byte{}, readURL: "https://storage.test/object?sig=x"}
	srv := &http.Server{Handler: helper}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return helper, NewRemote(socket)
}

func (f *fakeHelper) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.requests++
	status, hang := f.status, f.hang
	f.mu.Unlock()
	if hang != nil {
		<-hang
		return
	}
	if !strings.HasPrefix(request.RequestURI, "/v1/") {
		f.t.Errorf("request URI %q, want origin form under /v1/", request.RequestURI)
	}
	if status != 0 {
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"error":"helper says no"}`))
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/read-urls" {
		var req map[string]any
		if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.readReqs = append(f.readReqs, req)
		readURL := f.readURL
		f.mu.Unlock()
		_ = json.NewEncoder(writer).Encode(map[string]string{"url": readURL})
		return
	}
	name, ok := strings.CutPrefix(request.URL.Path, "/v1/objects/")
	if _, err := parseObjectName(name); !ok || err != nil {
		http.Error(writer, "bad name", http.StatusBadRequest)
		return
	}
	switch request.Method {
	case http.MethodPut:
		if f.earlyCreated {
			// Otherwise the server reads the rest of the body before answering.
			_ = http.NewResponseController(writer).EnableFullDuplex()
			data := make([]byte, 3)
			if _, err := io.ReadFull(request.Body, data); err != nil {
				return
			}
			f.mu.Lock()
			f.objects[name] = data
			f.mu.Unlock()
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{"size": len(data), "sha256": digest(data)})
			writer.(http.Flusher).Flush()
			return
		}
		data, err := io.ReadAll(request.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.received = max(f.received, len(data))
		if err != nil {
			return // incomplete: nothing is committed
		}
		if _, ok := f.objects[name]; ok {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		if f.strict {
			if limit, _ := strconv.Atoi(request.URL.Query().Get("max_bytes")); len(data) > limit {
				writer.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			if want := request.URL.Query().Get("sha256"); want != "" && want != digest(data) {
				writer.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
		}
		f.objects[name] = data
		f.commits++
		if f.failAfterCommit {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		sum := digest(data)
		if f.lieDigest {
			sum = digest([]byte("something else"))
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"size": len(data), "sha256": sum})
	case http.MethodHead, http.MethodGet:
		f.mu.Lock()
		data, ok := f.objects[name]
		f.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		writer.Header().Set("Object-SHA256", digest(data))
		if request.Method == http.MethodGet {
			_, _ = writer.Write(data)
		}
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, name)
		f.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestRemotePutStatOpenDelete(t *testing.T) {
	// The helper is reached over its socket only, never through a proxy.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	helper, rs := newFakeHelper(t)
	helper.strict = true
	ctx := context.Background()
	data := []byte("tar bytes")
	want := Object{Size: int64(len(data)), SHA256: digest(data)}

	obj, err := rs.Put(ctx, tenantKey, bytes.NewReader(data), PutOptions{MaxBytes: 100, ExpectedSHA256: want.SHA256})
	if err != nil || obj != want {
		t.Fatalf("Put = %+v, %v; want %+v", obj, err, want)
	}
	if got := helper.objects["tenants/"+testTenant+"/deps.tar"]; !bytes.Equal(got, data) {
		t.Fatalf("helper stored %q under the object name, have %v", got, helper.objects)
	}
	if _, err := rs.Put(ctx, tenantKey, bytes.NewReader(data), PutOptions{MaxBytes: 100}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}
	if got, err := rs.Stat(ctx, tenantKey); err != nil || got != want {
		t.Fatalf("Stat = %+v, %v; want %+v", got, err, want)
	}
	rc, got, err := rs.Open(ctx, tenantKey)
	if err != nil || got != want {
		t.Fatalf("Open = %+v, %v; want %+v", got, err, want)
	}
	content, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(content, data) {
		t.Fatalf("Open content = %q", content)
	}
	if err := rs.Delete(ctx, tenantKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := rs.Delete(ctx, tenantKey); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
	if _, err := rs.Stat(ctx, tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if _, _, err := rs.Open(ctx, tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
	}
}

// The fake commits any complete body, so nothing may be committed unless the
// API itself cut the upload off before its end.
func TestRemotePutCutsOffBeforeTheHelperCommits(t *testing.T) {
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
			helper, rs := newFakeHelper(t)
			if _, err := rs.Put(context.Background(), sharedKey, testCase.reader, testCase.opts); !errors.Is(err, testCase.want) {
				t.Fatalf("Put = %v, want %v", err, testCase.want)
			}
			helper.mu.Lock()
			defer helper.mu.Unlock()
			if helper.commits != 0 {
				t.Fatalf("helper committed %d objects", helper.commits)
			}
			if int64(helper.received) > testCase.opts.MaxBytes {
				t.Fatalf("helper received %d bytes, past MaxBytes %d", helper.received, testCase.opts.MaxBytes)
			}
		})
	}
}

func TestRemoteMapsHelperStatuses(t *testing.T) {
	ctx := context.Background()
	put := func(rs *Remote) error {
		_, err := rs.Put(ctx, sharedKey, strings.NewReader("x"), PutOptions{MaxBytes: 1})
		return err
	}
	stat := func(rs *Remote) error { _, err := rs.Stat(ctx, sharedKey); return err }
	cases := []struct {
		status int
		call   func(*Remote) error
		want   error
	}{
		{http.StatusConflict, put, ErrExists},
		{http.StatusRequestEntityTooLarge, put, ErrTooLarge},
		{http.StatusUnprocessableEntity, put, ErrDigestMismatch},
		{http.StatusNotFound, stat, ErrNotFound},
	}
	for _, testCase := range cases {
		helper, rs := newFakeHelper(t)
		helper.status = testCase.status
		if err := testCase.call(rs); !errors.Is(err, testCase.want) {
			t.Errorf("status %d: %v, want %v", testCase.status, err, testCase.want)
		}
	}

	helper, rs := newFakeHelper(t)
	helper.status = http.StatusServiceUnavailable
	for name, call := range map[string]func(*Remote) error{
		"Put":    put,
		"Stat":   stat,
		"Delete": func(rs *Remote) error { return rs.Delete(ctx, sharedKey) },
		"ReadURL": func(rs *Remote) error {
			_, err := rs.ReadURL(ctx, sharedKey, time.Minute)
			return err
		},
	} {
		err := call(rs)
		// A HEAD answer has no body to carry the message.
		if err == nil || !strings.Contains(err.Error(), "503") || (name != "Stat" && !strings.Contains(err.Error(), "helper says no")) {
			t.Errorf("%s with a failing helper = %v", name, err)
		}
	}
}

func TestRemoteRefusesAnAnswerThatDoesNotMatchTheUpload(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.lieDigest = true
	if _, err := rs.Put(context.Background(), sharedKey, strings.NewReader("data"), PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put accepted a digest other than the one sent")
	}
	if len(helper.objects) != 0 {
		t.Fatalf("mismatched object left behind: %v", helper.objects)
	}
}

func TestRemoteReadURL(t *testing.T) {
	helper, rs := newFakeHelper(t)
	ctx := context.Background()
	got, err := rs.ReadURL(ctx, tenantKey, 24*time.Hour)
	if err != nil || got != helper.readURL {
		t.Fatalf("ReadURL = %q, %v; want %q", got, err, helper.readURL)
	}
	if _, err := rs.ReadURL(ctx, tenantKey, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"name": "tenants/" + testTenant + "/deps.tar", "ttl_seconds": float64(3600)},
		{"name": "tenants/" + testTenant + "/deps.tar", "ttl_seconds": float64(1)},
	}
	for index, req := range helper.readReqs {
		for name, value := range want[index] {
			if req[name] != value {
				t.Errorf("request %d: %s = %v, want %v", index, name, req[name], value)
			}
		}
	}
	if _, err := rs.ReadURL(ctx, tenantKey, 0); err == nil {
		t.Error("ReadURL with a zero ttl succeeded")
	}
	for _, bad := range []string{"", "file:///etc/passwd", "/relative", "https://", "https://:443/x", "javascript:alert(1)",
		"http://storage.test/x", "https://user:pw@storage.test/x", "https://storage.test/x#y"} {
		helper.readURL = bad
		if _, err := rs.ReadURL(ctx, tenantKey, time.Minute); err == nil {
			t.Errorf("helper URL %q accepted", bad)
		}
	}
}

func TestRemoteRejectsInvalidKeysWithoutCallingTheHelper(t *testing.T) {
	helper, rs := newFakeHelper(t)
	ctx := context.Background()
	bad := Key{Scope: volume.SharedScope, ID: "../../x", Kind: volume.KindTar}
	if _, err := rs.Put(ctx, bad, strings.NewReader("x"), PutOptions{MaxBytes: 1}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Put = %v", err)
	}
	if _, err := rs.Stat(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Stat = %v", err)
	}
	if _, _, err := rs.Open(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Open = %v", err)
	}
	if err := rs.Delete(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Delete = %v", err)
	}
	if _, err := rs.ReadURL(ctx, bad, time.Minute); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ReadURL = %v", err)
	}
	if helper.requests != 0 {
		t.Fatalf("helper got %d requests", helper.requests)
	}
}

func TestRemoteWithoutHelper(t *testing.T) {
	rs := NewRemote(filepath.Join(t.TempDir(), "missing"))
	if _, err := rs.Stat(context.Background(), sharedKey); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat without a helper = %v, want a connection error", err)
	}
}

// The helper stores what has arrived and answers while the upload stalls:
// size and digest match what was read so far, only the missing end shows.
func TestRemotePutDiscardsAnObjectStoredBeforeTheUploadEnded(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.earlyCreated = true
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reader := io.MultiReader(strings.NewReader("abc"), gateReader{release})
	if _, err := rs.Put(context.Background(), sharedKey, reader, PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put accepted an object stored before the upload ended")
	}
	helper.mu.Lock()
	defer helper.mu.Unlock()
	if len(helper.objects) != 0 {
		t.Fatalf("early object left behind: %v", helper.objects)
	}
}

// After the whole upload went out, a failed answer leaves the outcome open:
// the object decides.
func TestRemotePutSettlesAnOpenOutcome(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.failAfterCommit = true
	data := []byte("data")
	obj, err := rs.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10})
	if want := (Object{Size: 4, SHA256: digest(data)}); err != nil || obj != want {
		t.Fatalf("Put = %+v, %v; want the stored object %+v", obj, err, want)
	}

	helper, rs = newFakeHelper(t)
	helper.status = http.StatusInternalServerError
	if _, err := rs.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put succeeded although nothing was stored")
	}
}

func TestRemoteGivesUpOnAStuckHelper(t *testing.T) {
	previous := remoteIdleTimeout
	remoteIdleTimeout = remoteIdleTimeoutForTest
	helper, rs := newFakeHelper(t)
	remoteIdleTimeout = previous
	helper.hang = make(chan struct{})
	t.Cleanup(func() { close(helper.hang) })

	start := time.Now()
	if _, err := rs.Stat(context.Background(), sharedKey); err == nil {
		t.Fatal("Stat on a stuck helper succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Stat took %s on a stuck helper", elapsed)
	}
}

// The idle timeout ends stalls only: an upload and a download that keep
// moving, and a consumer that pauses between reads, outlive it.
func TestRemoteIdleTimeoutSparesSlowTransfers(t *testing.T) {
	previous := remoteIdleTimeout
	remoteIdleTimeout = remoteIdleTimeoutForTest
	_, rs := newFakeHelper(t)
	remoteIdleTimeout = previous
	ctx := context.Background()

	slow := readerFunc(func(buf []byte) (int, error) {
		time.Sleep(80 * time.Millisecond)
		buf[0] = 'x'
		return 1, nil
	})
	if _, err := rs.Put(ctx, sharedKey, io.LimitReader(slow, 8), PutOptions{MaxBytes: 8}); err != nil {
		t.Fatalf("slow upload: %v", err)
	}

	big := bytes.Repeat([]byte("volume"), 1<<20)
	if _, err := rs.Put(ctx, tenantKey, bytes.NewReader(big), PutOptions{MaxBytes: int64(len(big))}); err != nil {
		t.Fatal(err)
	}
	rc, _, err := rs.Open(ctx, tenantKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	head := make([]byte, 1024)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * remoteIdleTimeoutForTest)
	rest, err := io.ReadAll(rc)
	if err != nil || len(head)+len(rest) != len(big) {
		t.Fatalf("download after a pause: %d bytes, %v", len(head)+len(rest), err)
	}
}

const remoteIdleTimeoutForTest = 200 * time.Millisecond

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(buf []byte) (int, error) { return f(buf) }
