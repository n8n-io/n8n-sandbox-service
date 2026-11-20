package api

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const (
	sharedAdminKey     = "admin-key"
	sharedPublisherKey = "publisher-key"
)

type sharedVolumeEnv struct {
	router    http.Handler
	tenantKey string
	storeDir  string
	// shutdown ends the shared volume service's context, as the API's
	// shutdown does.
	shutdown context.CancelFunc
}

// newSharedVolumeEnv returns a gateway with a local volume store, or none
// when withStore is false, and a tenant key.
func newSharedVolumeEnv(t *testing.T, withStore bool) *sharedVolumeEnv {
	t.Helper()
	sqlite, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	dataDir := t.TempDir()
	serviceCtx, shutdown := context.WithCancel(context.Background())
	t.Cleanup(shutdown)
	var service *volumes.Service
	if withStore {
		local, err := volumestore.NewLocal(dataDir, "https://api.test:8443")
		if err != nil {
			t.Fatal(err)
		}
		service = volumes.NewService(serviceCtx, sqlite, local, metrics.NewSharedVolumeRecorder(metrics.NewAPIRecorder(false)))
	}
	router, err := NewGatewayRouter(sqlite, withRunnerTLS(&config.APIConfig{
		APIKeys:              map[string]struct{}{sharedAdminKey: {}},
		ProvisionerKeys:      map[string]struct{}{"provisioner-key": {}},
		ScalerKeys:           map[string]struct{}{"scaler-key": {}},
		PublisherKeys:        map[string]struct{}{sharedPublisherKey: {}},
		DefaultMaxSandboxes:  10,
		MaxInflightPerTenant: 4,
	}), registry.New(45*time.Second), metrics.NewAPIRecorder(true), NewActivityRecorder(sqlite, nil), WithSharedVolumes(service))
	if err != nil {
		t.Fatal(err)
	}
	return &sharedVolumeEnv{
		router:    router,
		tenantKey: mintTenantKey(t, router, `{"name":"shared-volume-tenant"}`).Key.APIKey,
		storeDir:  filepath.Join(dataDir, "volumes"),
		shutdown:  shutdown,
	}
}

func (env *sharedVolumeEnv) do(t *testing.T, method, target, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)
	return recorder
}

func (env *sharedVolumeEnv) storedFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	_ = filepath.Walk(env.storeDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func sharedTar(t *testing.T, files ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, name := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sharedDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sharedPath(data []byte) string {
	return "/shared-volumes/sha256:" + sharedDigest(data)
}

func reasonOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body.Reason
}

func TestSharedVolumeRoutesTakeAdminAndPublisherKeysOnly(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	data := sharedTar(t, "a")
	path := sharedPath(data)

	for _, key := range []string{env.tenantKey, "provisioner-key", "scaler-key"} {
		for _, request := range []struct {
			method, target string
		}{
			{http.MethodPut, path}, {http.MethodGet, path}, {http.MethodHead, path},
			{http.MethodDelete, path}, {http.MethodGet, "/shared-volumes"},
		} {
			if got := env.do(t, request.method, request.target, key, data); got.Code != http.StatusForbidden {
				t.Errorf("%s %s with a %s key = %d, want 403", request.method, request.target, keyKind(env, key), got.Code)
			}
		}
	}
	if files := env.storedFiles(t); len(files) != 0 {
		t.Fatalf("store holds %v after refused requests", files)
	}

	for _, key := range []string{sharedAdminKey, sharedPublisherKey} {
		other := sharedTar(t, "by-"+key)
		if got := env.do(t, http.MethodPut, sharedPath(other), key, other); got.Code != http.StatusCreated {
			t.Fatalf("PUT with %s = %d %s, want 201", key, got.Code, got.Body.String())
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if got := env.do(t, method, sharedPath(other), key, nil); got.Code != http.StatusOK {
				t.Errorf("%s with %s = %d, want 200", method, key, got.Code)
			}
		}
		if got := env.do(t, http.MethodGet, "/shared-volumes", key, nil); got.Code != http.StatusOK {
			t.Errorf("list with %s = %d, want 200", key, got.Code)
		}
		if got := env.do(t, http.MethodDelete, sharedPath(other), key, nil); got.Code != http.StatusNoContent {
			t.Errorf("DELETE with %s = %d, want 204", key, got.Code)
		}
	}
}

func keyKind(env *sharedVolumeEnv, key string) string {
	if key == env.tenantKey {
		return "tenant"
	}
	return key
}

func TestPublisherKeyReachesNothingElse(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	for _, request := range []struct {
		method, target string
	}{
		{http.MethodGet, "/sandboxes"},
		{http.MethodPost, "/sandboxes"},
		{http.MethodPost, "/code/sandboxes"},
		{http.MethodGet, "/sandboxes/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/admin/tenants"},
		{http.MethodPost, "/admin/tenants"},
		{http.MethodGet, "/admin/runners"},
		{http.MethodGet, "/admin/scaler"},
		{http.MethodGet, "/shared-volumesX"},
		{http.MethodGet, "/shared-volumes/../sandboxes"},
		{http.MethodGet, "/shared-volumes/"},
	} {
		got := env.do(t, request.method, request.target, sharedPublisherKey, nil)
		if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "publisher API key may only manage shared volumes") {
			t.Errorf("%s %s with a publisher key = %d %s, want 403", request.method, request.target, got.Code, got.Body.String())
		}
	}
}

func TestSharedVolumeRoutesWithoutAStore(t *testing.T) {
	env := newSharedVolumeEnv(t, false)
	data := sharedTar(t, "a")
	for _, request := range []struct {
		method, target string
	}{
		{http.MethodPut, sharedPath(data)}, {http.MethodGet, sharedPath(data)},
		{http.MethodDelete, sharedPath(data)}, {http.MethodGet, "/shared-volumes"},
	} {
		got := env.do(t, request.method, request.target, sharedPublisherKey, data)
		if got.Code != http.StatusNotImplemented || reasonOf(t, got) != reasonVolumeStoreDisabled {
			t.Errorf("%s %s = %d %s, want 501 %s", request.method, request.target, got.Code, got.Body.String(), reasonVolumeStoreDisabled)
		}
	}
	if got := env.do(t, http.MethodGet, "/shared-volumes", env.tenantKey, nil); got.Code != http.StatusForbidden {
		t.Errorf("list with a tenant key and no store = %d, want 403", got.Code)
	}
}

func TestPutSharedVolume(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	data := sharedTar(t, "docs/readme", "bin/run")
	path := sharedPath(data)

	created := env.do(t, http.MethodPut, path+"?name=n8n-assistant-2.45.5", sharedPublisherKey, data)
	if created.Code != http.StatusCreated || created.Header().Get("Location") != path {
		t.Fatalf("PUT = %d %s, Location %q; want 201 and %s", created.Code, created.Body.String(), created.Header().Get("Location"), path)
	}
	var body sharedVolumeResponse
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Digest != "sha256:"+sharedDigest(data) || body.Name != "n8n-assistant-2.45.5" || body.Bytes != int64(len(data)) ||
		body.Tar.Entries != 4 || body.Tar.ContentBytes != 4 || body.Tar.DecompressedBytes != int64(len(data)) || body.Image != nil || body.PublishedAt == 0 {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(created.Body.String(), "image") {
		t.Errorf("body %s has an image without a builder", created.Body.String())
	}

	if again := env.do(t, http.MethodPut, path, sharedPublisherKey, data); again.Code != http.StatusOK {
		t.Fatalf("repeated PUT = %d %s, want 200", again.Code, again.Body.String())
	}
	if got := env.do(t, http.MethodGet, path, sharedPublisherKey, nil); got.Code != http.StatusOK || got.Body.String() != created.Body.String() {
		t.Fatalf("GET = %d %s, want the PUT's body", got.Code, got.Body.String())
	}
	if got := env.do(t, http.MethodGet, "/shared-volumes?name=n8n-assistant-2.45.5", sharedPublisherKey, nil); got.Code != http.StatusOK || !strings.HasPrefix(got.Body.String(), "[{") {
		t.Fatalf("list by name = %d %s", got.Code, got.Body.String())
	}
	if got := env.do(t, http.MethodGet, "/shared-volumes?name=other", sharedPublisherKey, nil); got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != "[]" {
		t.Fatalf("list by an unknown name = %d %s, want []", got.Code, got.Body.String())
	}

	other := sharedTar(t, "other")
	for name, tc := range map[string]struct {
		target string
		body   []byte
		status int
		reason string
	}{
		"taken name":         {sharedPath(other) + "?name=n8n-assistant-2.45.5", other, http.StatusConflict, reasonNameTaken},
		"another name":       {path + "?name=another", data, http.StatusConflict, reasonNameMismatch},
		"wrong digest":       {sharedPath(data[:100]), data, http.StatusUnprocessableEntity, reasonDigestMismatch},
		"invalid tar":        {sharedPath([]byte("junk")), []byte("junk"), http.StatusUnprocessableEntity, reasonInvalidTar},
		"empty body":         {sharedPath(nil), []byte{}, http.StatusUnprocessableEntity, reasonInvalidTar},
		"invalid name":       {sharedPath(other) + "?name=Not_valid!", other, http.StatusBadRequest, ""},
		"name twice":         {sharedPath(other) + "?name=a&name=b", other, http.StatusBadRequest, ""},
		"unknown parameter":  {sharedPath(other) + "?build=no", other, http.StatusBadRequest, ""},
		"uppercase digest":   {"/shared-volumes/sha256:" + strings.ToUpper(sharedDigest(other)), other, http.StatusBadRequest, ""},
		"no sha256: prefix":  {"/shared-volumes/" + sharedDigest(other), other, http.StatusBadRequest, ""},
		"short digest":       {"/shared-volumes/sha256:abc", other, http.StatusBadRequest, ""},
		"another algorithm":  {"/shared-volumes/sha512:" + sharedDigest(other), other, http.StatusBadRequest, ""},
		"escaped colon okay": {"/shared-volumes/sha256%3A" + sharedDigest(other), other, http.StatusCreated, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := env.do(t, http.MethodPut, tc.target, sharedPublisherKey, tc.body)
			if got.Code != tc.status || (tc.reason != "" && reasonOf(t, got) != tc.reason) {
				t.Fatalf("PUT %s = %d %s, want %d %s", tc.target, got.Code, got.Body.String(), tc.status, tc.reason)
			}
		})
	}
}

func TestPutSharedVolumeNeedsALength(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	data := sharedTar(t, "a")
	req := httptest.NewRequest(http.MethodPut, sharedPath(data), io.NopCloser(bytes.NewReader(data)))
	req.ContentLength = -1
	req.Header.Set("X-Api-Key", sharedPublisherKey)
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusLengthRequired {
		t.Fatalf("PUT without Content-Length = %d, want 411", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodPut, sharedPath(data), failingBody{t})
	req.ContentLength = volumes.MaxTarBytes + 1
	req.Header.Set("X-Api-Key", sharedPublisherKey)
	recorder = httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge || reasonOf(t, recorder) != reasonTooLarge {
		t.Fatalf("PUT over the limit = %d %s, want 413 %s", recorder.Code, recorder.Body.String(), reasonTooLarge)
	}
}

type failingBody struct{ t *testing.T }

func (body failingBody) Read([]byte) (int, error) {
	body.t.Error("the body was read")
	return 0, io.EOF
}

func TestGetAndDeleteSharedVolume(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	data := sharedTar(t, "a")
	path := sharedPath(data)
	if got := env.do(t, http.MethodGet, path, sharedAdminKey, nil); got.Code != http.StatusNotFound {
		t.Fatalf("GET before PUT = %d, want 404", got.Code)
	}
	if got := env.do(t, http.MethodHead, path, sharedAdminKey, nil); got.Code != http.StatusNotFound {
		t.Fatalf("HEAD before PUT = %d, want 404", got.Code)
	}
	if got := env.do(t, http.MethodPut, path, sharedAdminKey, data); got.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
	}
	if got := env.do(t, http.MethodHead, path, sharedAdminKey, nil); got.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", got.Code)
	}
	if got := env.do(t, http.MethodDelete, path, sharedPublisherKey, nil); got.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", got.Code, got.Body.String())
	}
	if files := env.storedFiles(t); len(files) != 0 {
		t.Fatalf("store holds %v after DELETE", files)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if got := env.do(t, method, path, sharedPublisherKey, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s after DELETE = %d, want 404", method, got.Code)
		}
	}
	if got := env.do(t, http.MethodGet, "/shared-volumes/sha256:nope", sharedPublisherKey, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("GET of a malformed digest = %d, want 400", got.Code)
	}
}

func TestSharedVolumeErrorAnswers(t *testing.T) {
	for _, tc := range []struct {
		err        error
		status     int
		reason     string
		retryAfter bool
	}{
		{volumes.ErrNotFound, http.StatusNotFound, "", false},
		{volumes.ErrPublishInProgress, http.StatusConflict, reasonPublishInProgress, true},
		{volumes.ErrNameTaken, http.StatusConflict, reasonNameTaken, false},
		{volumes.ErrNameMismatch, http.StatusConflict, reasonNameMismatch, false},
		{volumes.ErrVolumeLimit, http.StatusConflict, reasonSharedVolumeLimit, false},
		{volumes.ErrPublishLimit, http.StatusTooManyRequests, reasonPublishLimit, true},
		{fmt.Errorf("%w: limit", volumes.ErrTooLarge), http.StatusRequestEntityTooLarge, reasonTooLarge, false},
		{fmt.Errorf("%w: rule", volumes.ErrInvalidTar), http.StatusUnprocessableEntity, reasonInvalidTar, false},
		{volumes.ErrDigestMismatch, http.StatusUnprocessableEntity, reasonDigestMismatch, false},
		{fmt.Errorf("%w: %w", volumes.ErrUpload, os.ErrDeadlineExceeded), http.StatusRequestTimeout, "", false},
		{fmt.Errorf("%w: reset", volumes.ErrUpload), http.StatusBadRequest, "", false},
		{fmt.Errorf("%w: lease", volumes.ErrInterrupted), http.StatusServiceUnavailable, reasonPublishInterrupted, false},
		{errors.New("store: /var/lib/secret/path"), http.StatusInternalServerError, "", false},
	} {
		recorder := httptest.NewRecorder()
		writeSharedVolumeError(recorder, httptest.NewRequest(http.MethodPut, "/shared-volumes/x", nil), tc.err)
		var body APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != tc.status || body.Reason != tc.reason || (recorder.Header().Get("Retry-After") != "") != tc.retryAfter {
			t.Errorf("%v: %d %s Retry-After %q; want %d %q", tc.err, recorder.Code, recorder.Body.String(), recorder.Header().Get("Retry-After"), tc.status, tc.reason)
		}
		if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "reset") {
			t.Errorf("%v: body %s passes the error's text on", tc.err, recorder.Body.String())
		}
	}
}

// newSharedVolumeServer serves env's router like cmd/api, with a short
// ReadTimeout and stall timeout.
func newSharedVolumeServer(t *testing.T, env *sharedVolumeEnv, readTimeout, stall time.Duration) *httptest.Server {
	t.Helper()
	previous := uploadStallTimeout
	uploadStallTimeout = stall
	t.Cleanup(func() { uploadStallTimeout = previous })
	server := httptest.NewUnstartedServer(env.router)
	server.Config.ReadTimeout = readTimeout
	server.Config.ReadHeaderTimeout = readTimeout
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// An upload may take longer than the server's ReadTimeout while it keeps
// sending.
func TestSharedVolumeUploadOutlastsTheReadTimeout(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	server := newSharedVolumeServer(t, env, 300*time.Millisecond, 400*time.Millisecond)
	data := sharedTar(t, "a", "b", "c", "d")
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		for offset := 0; offset < len(data); offset += 1024 {
			time.Sleep(100 * time.Millisecond)
			if _, err := pipeWriter.Write(data[offset:min(offset+1024, len(data))]); err != nil {
				return
			}
		}
		pipeWriter.Close()
	}()
	req, err := http.NewRequest(http.MethodPut, server.URL+sharedPath(data), pipeReader)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("X-Api-Key", sharedPublisherKey)
	start := time.Now()
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		answer, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT = %d %s, want 201", resp.StatusCode, answer)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("the upload took %s, so it did not outlast the ReadTimeout", elapsed)
	}
}

// An upload that stops sending ends with 408 and leaves nothing.
func TestStalledSharedVolumeUpload(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	server := newSharedVolumeServer(t, env, 5*time.Second, 200*time.Millisecond)
	data := sharedTar(t, "a", "b", "c", "d")
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: api\r\nX-Api-Key: %s\r\nContent-Length: %d\r\n\r\n", sharedPath(data), sharedPublisherKey, len(data))
	if _, err := conn.Write(data[:1024]); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled PUT = %d, want 408", resp.StatusCode)
	}
	if files := env.storedFiles(t); len(files) != 0 {
		t.Fatalf("store holds %v after a stalled upload", files)
	}
}

// Shutdown ends a publish whose upload waits for the client at once, not when
// the upload stalls.
func TestSharedVolumeUploadEndsAtShutdown(t *testing.T) {
	env := newSharedVolumeEnv(t, true)
	server := newSharedVolumeServer(t, env, time.Minute, time.Minute)
	data := sharedTar(t, "a", "b", "c", "d")
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: api\r\nX-Api-Key: %s\r\nContent-Length: %d\r\n\r\n", sharedPath(data), sharedPublisherKey, len(data))
	if _, err := conn.Write(data[:1024]); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(env.storedFiles(t)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upload never reached the store")
		}
		time.Sleep(5 * time.Millisecond)
	}
	env.shutdown()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	defer resp.Body.Close()
	var answer APIError
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || answer.Reason != reasonPublishInterrupted {
		t.Fatalf("PUT at shutdown = %d %q, want 503 %q", resp.StatusCode, answer.Reason, reasonPublishInterrupted)
	}
	if files := env.storedFiles(t); len(files) != 0 {
		t.Fatalf("store holds %v after an interrupted upload", files)
	}
}
