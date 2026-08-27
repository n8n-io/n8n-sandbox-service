package runner

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withPeerCert marks a request as having arrived over a TLS connection that
// presented a client certificate. The middleware only checks that one is
// present; verifying it against the CA is the TLS stack's job.
func withPeerCert(req *http.Request) *http.Request {
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	return req
}

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

	// Carries a client certificate, so the API key is the only thing left to fail on.
	req := withPeerCert(httptest.NewRequest(http.MethodGet, "/sandboxes/test", nil))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected auth failure, got status %d", rec.Code)
	}
}

func TestAuthMiddlewareRequiresClientCertificate(t *testing.T) {
	handler := AuthMiddleware(map[string]struct{}{"secret": {}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// A valid API key is not enough on its own: that is the whole point of
	// putting mTLS in front of the listener.
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/test", nil)
	req.Header.Set("X-Api-Key", "secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected auth failure without a client certificate, got status %d", rec.Code)
	}
}

func TestAuthMiddlewareAcceptsCertificateAndAPIKey(t *testing.T) {
	handler := AuthMiddleware(map[string]struct{}{"secret": {}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := withPeerCert(httptest.NewRequest(http.MethodGet, "/sandboxes/test", nil))
	req.Header.Set("X-Api-Key", "secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected request to be allowed, got status %d", rec.Code)
	}
}
