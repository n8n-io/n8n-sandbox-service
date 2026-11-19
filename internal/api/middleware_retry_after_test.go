package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetryAfterMiddleware(t *testing.T) {
	for _, tc := range []struct {
		code     int
		preset   string
		want     string
		implicit bool
	}{
		{code: http.StatusServiceUnavailable, want: "1"},
		{code: http.StatusTooManyRequests, want: "1"},
		{code: http.StatusServiceUnavailable, preset: "5", want: "5"},
		{code: http.StatusBadGateway, want: ""},
		{code: http.StatusOK, want: "", implicit: true},
	} {
		h := RetryAfterMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.preset != "" {
				w.Header().Set("Retry-After", tc.preset)
			}
			if !tc.implicit {
				w.WriteHeader(tc.code)
			}
			_, _ = w.Write([]byte("x"))
		}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != tc.code || rr.Header().Get("Retry-After") != tc.want {
			t.Errorf("%d preset %q: got %d Retry-After %q, want %q", tc.code, tc.preset, rr.Code, rr.Header().Get("Retry-After"), tc.want)
		}
	}
}

// A runner's 503 relayed through the proxy gains a Retry-After.
func TestProxiedRunner503GetsRetryAfter(t *testing.T) {
	h := RetryAfterMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "daemon temporarily unavailable")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sandboxes/x/executions", nil))
	if rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", rr.Header().Get("Retry-After"))
	}
}
