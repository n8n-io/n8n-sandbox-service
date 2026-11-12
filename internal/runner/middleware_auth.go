package runner

import (
	"crypto/subtle"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/probes"
)

// publicPaths skip auth and the access log.
var publicPaths = probes.New("/healthz", "/livez", "/readyz", "/metrics")

// AuthMiddleware requires a client certificate signed by the configured CA, and
// a valid API key.
//
// The listener negotiates TLS with VerifyClientCertIfGiven, so a peer that sent
// no certificate still completes the handshake and reaches here; requiring one
// is this middleware's job. GET and HEAD on the health endpoints are always
// allowed through, since probes cannot present a certificate; /metrics is also
// unauthenticated when it's mounted (operators are expected to firewall the
// port).
func AuthMiddleware(apiKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if publicPaths.Public(r) {
				next.ServeHTTP(w, r)
				return
			}

			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				writeError(w, http.StatusUnauthorized, "client certificate required")
				return
			}

			apiKey := r.Header.Get("X-Api-Key")
			if apiKey == "" {
				writeError(w, http.StatusUnauthorized, "missing API key")
				return
			}
			if !constantTimeContains(apiKeys, apiKey) {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			grpctls.NotePeer(r.TLS.PeerCertificates[0])
			next.ServeHTTP(w, r)
		})
	}
}

// constantTimeContains checks if key exists in the allowed set using constant-time comparison.
func constantTimeContains(allowed map[string]struct{}, key string) bool {
	for allowedKey := range allowed {
		if subtle.ConstantTimeCompare([]byte(allowedKey), []byte(key)) == 1 {
			return true
		}
	}
	return false
}
