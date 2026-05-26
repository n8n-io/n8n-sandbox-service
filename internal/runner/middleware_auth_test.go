package runner

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddlewareAllowsHealthEndpointsWithoutAPIKey(t *testing.T) {
	handler := AuthMiddleware(map[string]struct{}{"secret": {}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/healthz", "/livez", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected %s to bypass auth, got status %d", path, rec.Code)
			}
		})
	}
}

func TestAuthMiddlewareRequiresAPIKeyForOtherPaths(t *testing.T) {
	handler := AuthMiddleware(map[string]struct{}{"secret": {}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected auth failure, got status %d", rec.Code)
	}
}
