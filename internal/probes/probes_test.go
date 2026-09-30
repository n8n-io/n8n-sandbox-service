package probes

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublic(t *testing.T) {
	p := New("/healthz", "/metrics")
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/healthz", true},
		{http.MethodHead, "/metrics", true},
		{http.MethodPost, "/healthz", false},
		{"FOO", "/metrics", false},
		{http.MethodGet, "/sandboxes", false},
		{http.MethodGet, "/healthz/", false},
	}
	for _, c := range cases {
		if got := p.Public(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("Public(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestRejectOtherMethods(t *testing.T) {
	cases := []struct {
		method, path string
		wantNext     bool
	}{
		{http.MethodGet, "/healthz", true},
		{http.MethodHead, "/healthz", true},
		{http.MethodPost, "/healthz", false},
		{"FOO", "/healthz", false},
		{http.MethodPost, "/sandboxes", true},
		{"FOO", "/sandboxes", true},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			called := false
			handler := New("/healthz").RejectOtherMethods(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))

			if called != c.wantNext {
				t.Fatalf("next called = %v, want %v", called, c.wantNext)
			}
			if c.wantNext {
				return
			}
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
			}
			if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
		})
	}
}
