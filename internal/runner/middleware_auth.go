package runner

import "net/http"

// AuthMiddleware requires a client certificate signed by the configured CA, and
// a valid API key.
//
// The listener negotiates TLS with VerifyClientCertIfGiven, so a peer that sent
// no certificate still completes the handshake and reaches here; requiring one
// is this middleware's job. Health endpoints are always allowed through, since
// probes cannot present a certificate; /metrics is also unauthenticated when
// it's mounted (operators are expected to firewall the port).
func AuthMiddleware(apiKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz", "/livez", "/readyz", "/metrics":
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
			if _, ok := apiKeys[apiKey]; !ok {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
