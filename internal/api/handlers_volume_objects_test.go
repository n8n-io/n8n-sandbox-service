package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

const volumeTenant = "0b6f3a52-7c1e-4d2a-9f3b-5e8d1c2a4b6f"

var volumeKey = volumestore.Key{Scope: volumeTenant, ID: "deps", Kind: volume.KindTar}

// newVolumeTestGateway returns the public router.
func newVolumeTestGateway(t *testing.T) http.Handler {
	t.Helper()
	sandboxStore, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = sandboxStore.Close() })
	router, err := NewGatewayRouter(sandboxStore, &config.APIConfig{
		APIKeys:              map[string]struct{}{"admin-key": {}},
		MaxFileBytes:         1024,
		MaxInflightPerTenant: 4,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(true), NewActivityRecorder(sandboxStore, nil))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router
}

func newVolumeTestLocal(t *testing.T) *volumestore.Local {
	t.Helper()
	vs, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// newVolumeObjectsTestRouter returns the volume-objects listener's router.
func newVolumeObjectsTestRouter(vs *volumestore.Local, maxInflight int) http.Handler {
	return NewVolumeObjectsRouter(vs, maxInflight, metrics.NewAPIRecorder(true))
}

// get sends a GET for rawURL, with an API key when key is set.
func get(t *testing.T, handler http.Handler, rawURL, key string) *httptest.ResponseRecorder {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestVolumeObjectsRouterServesSignedURLsWithoutAPIKey(t *testing.T) {
	vs := newVolumeTestLocal(t)
	router := newVolumeObjectsTestRouter(vs, 4)
	ctx := context.Background()
	if _, err := vs.Put(ctx, volumeKey, strings.NewReader("tar bytes"), volumestore.PutOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}
	signed, err := vs.ReadURL(ctx, volumeKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	rr := get(t, router, signed, "")
	if rr.Code != http.StatusOK || rr.Body.String() != "tar bytes" {
		t.Fatalf("signed GET: %d %q, want 200 \"tar bytes\"", rr.Code, rr.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	parsed, _ := url.Parse(signed)
	if rr := get(t, router, parsed.Path, ""); rr.Code != http.StatusForbidden {
		t.Errorf("unsigned GET: %d, want 403", rr.Code)
	}
	// An API key is no substitute for the signature.
	if rr := get(t, router, parsed.Path, "admin-key"); rr.Code != http.StatusForbidden {
		t.Errorf("unsigned GET with an admin key: %d, want 403", rr.Code)
	}
	query := parsed.Query()
	query.Set("expires", "4102444800")
	if rr := get(t, router, parsed.Path+"?"+query.Encode(), ""); rr.Code != http.StatusForbidden {
		t.Errorf("tampered expiry: %d, want 403", rr.Code)
	}

	missing, err := vs.ReadURL(ctx, volumestore.Key{Scope: volume.SharedScope, ID: "nope", Kind: volume.KindExt4}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rr := get(t, router, missing, ""); rr.Code != http.StatusNotFound {
		t.Errorf("missing object: %d, want 404", rr.Code)
	}

	req := httptest.NewRequest(http.MethodDelete, parsed.RequestURI(), nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on a signed URL: %d, want 405", rr.Code)
	}
}

// The listener's router has none of the public API's routes, with or without
// an API key.
func TestVolumeObjectsRouterServesNothingElse(t *testing.T) {
	router := newVolumeObjectsTestRouter(newVolumeTestLocal(t), 4)
	for _, target := range []string{"/", "/healthz", "/metrics", "/sandboxes", "/admin/tenants"} {
		for _, key := range []string{"", "admin-key"} {
			if rr := get(t, router, target, key); rr.Code != http.StatusNotFound {
				t.Errorf("GET %s with key %q: %d, want 404", target, key, rr.Code)
			}
		}
	}
}

// The public router answers a signed URL like any path it has no route for.
func TestGatewayRouterHasNoVolumeObjectRoute(t *testing.T) {
	vs := newVolumeTestLocal(t)
	if _, err := vs.Put(context.Background(), volumeKey, strings.NewReader("tar bytes"), volumestore.PutOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}
	signed, err := vs.ReadURL(context.Background(), volumeKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router := newVolumeTestGateway(t)
	for _, key := range []string{"", "admin-key"} {
		unknown := get(t, router, "/no-such-route", key)
		got := get(t, router, signed, key)
		if got.Code != unknown.Code || got.Body.String() != unknown.Body.String() {
			t.Errorf("signed GET with key %q: %d %q, want it answered like an unknown path: %d %q",
				key, got.Code, got.Body.String(), unknown.Code, unknown.Body.String())
		}
		if got.Code == http.StatusOK {
			t.Errorf("signed GET with key %q was served", key)
		}
	}
	if rr := get(t, router, signed, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("signed GET without a key: %d, want 401", rr.Code)
	}
	if rr := get(t, router, signed, "admin-key"); rr.Code != http.StatusNotFound {
		t.Errorf("signed GET with an admin key: %d, want 404", rr.Code)
	}
}

func TestVolumeObjectRouteCountsDownloadsPerScope(t *testing.T) {
	vs := newVolumeTestLocal(t)
	if _, err := vs.Put(context.Background(), volumeKey, strings.NewReader("x"), volumestore.PutOptions{MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	signed, err := vs.ReadURL(context.Background(), volumeKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	limiter := limits.NewKeyedLimiter(1)
	handler := handleVolumeObject(vs, limiter, metrics.NewAPIRecorder(false))

	limiter.TryAcquire(volumeTenant)
	if rr := get(t, handler, signed, ""); rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") != limitRetryAfter {
		t.Fatalf("at the limit: %d, want 429 with Retry-After", rr.Code)
	}
	limiter.Release(volumeTenant)
	if rr := get(t, handler, signed, ""); rr.Code != http.StatusOK {
		t.Fatalf("under the limit: %d, want 200", rr.Code)
	}
}

// The server has no write timeout, so without the stall timeout a client that
// stops reading would hold its slot for as long as it keeps the connection.
func TestVolumeObjectStalledDownloadGivesBackItsSlot(t *testing.T) {
	previous := volumeObjectStallTimeout
	volumeObjectStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { volumeObjectStallTimeout = previous })

	vs := newVolumeTestLocal(t)
	const size = 32 << 20 // more than the socket buffers on both ends hold
	if _, err := vs.Put(context.Background(), volumeKey, io.LimitReader(zeroReader{}, size), volumestore.PutOptions{MaxBytes: size}); err != nil {
		t.Fatal(err)
	}
	signed, err := vs.ReadURL(context.Background(), volumeKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newVolumeObjectsTestRouter(vs, 1))
	defer srv.Close()

	parsed, _ := url.Parse(signed)
	stalled, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if _, err := fmt.Fprintf(stalled, "GET %s HTTP/1.1\r\nHost: api.test\r\n\r\n", parsed.RequestURI()); err != nil {
		t.Fatal(err)
	}
	// Once the status line is back the download holds the slot; then stop reading.
	if status, err := bufio.NewReader(stalled).ReadString('\n'); err != nil || !strings.Contains(status, "200") {
		t.Fatalf("stalled download status %q, %v", status, err)
	}

	sawLimit := false
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(srv.URL + parsed.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		received, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && received == size {
			break
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("got %d with %d bytes, want 429 or the whole object", resp.StatusCode, received)
		}
		sawLimit = true
		if time.Now().After(deadline) {
			t.Fatal("the stalled download still holds its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawLimit {
		t.Fatal("the stalled download never held the slot, so the test proves nothing")
	}
}

type zeroReader struct{}

func (zeroReader) Read(buf []byte) (int, error) {
	clear(buf)
	return len(buf), nil
}

// A signed URL sent to the public listener by mistake must not end up in its
// log either.
func TestVolumeObjectSignatureStaysOutOfAccessLog(t *testing.T) {
	vs := newVolumeTestLocal(t)
	signed, err := vs.ReadURL(context.Background(), volumeKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for name, router := range map[string]http.Handler{
		"volume objects listener": newVolumeObjectsTestRouter(vs, 4),
		"public listener":         newVolumeTestGateway(t),
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			get(t, router, signed, "")
			get(t, router, strings.Replace(signed, "?", "?%73ig=x&", 1), "")

			events := logs()
			if len(events) != 2 {
				t.Fatalf("got %d log events, want 2", len(events))
			}
			for _, event := range events {
				query, _ := event["query"].(string)
				if query != "REDACTED" {
					t.Errorf("logged query %q", query)
				}
			}
		})
	}
}

func TestRedactSignature(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "",
		"path=/tmp":                  "path=/tmp",
		"signature=x&design=1":       "signature=x&design=1",
		"expires=1&sig=abc":          "REDACTED",
		"expires=1&sig=abc&copy=abc": "REDACTED",
		"expires=1&sig=abc&abc":      "REDACTED",
		"%73ig=abc&signature=x":      "REDACTED",
		"sig":                        "REDACTED",
		"design=1&sig=abc&x=%zz":     "REDACTED",
		"expires=1;sig=abc":          "REDACTED",
		"expires=1&+sig=abc":         "REDACTED",
		"expires=1&%20sig%20=abc":    "REDACTED",
		"a=1;b=2":                    "a=1;b=2",
	} {
		if got := redactSignature(raw); got != want {
			t.Errorf("redactSignature(%q) = %q, want %q", raw, got, want)
		}
	}
}
