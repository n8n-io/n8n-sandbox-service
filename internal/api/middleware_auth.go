package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

type authRole string

const (
	roleAdmin  authRole = "admin"
	roleTenant authRole = "tenant"
)

type authIdentity struct {
	Role     authRole
	TenantID string
}

type authContextKey struct{}

func withAuthIdentity(ctx context.Context, id authIdentity) context.Context {
	return context.WithValue(ctx, authContextKey{}, id)
}

func authFromContext(ctx context.Context) (authIdentity, bool) {
	id, ok := ctx.Value(authContextKey{}).(authIdentity)
	return id, ok
}

func hashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func apiKeyPrefix(plaintext string) string {
	// Keys look like sbk_<8hex>_<secret>; fall back to first 8 chars.
	const marker = "sbk_"
	if len(plaintext) >= 12 && plaintext[:4] == marker {
		return plaintext[4:12]
	}
	if len(plaintext) >= 8 {
		return plaintext[:8]
	}
	return plaintext
}

// AuthMiddleware checks X-Api-Key against env admin keys, then DB-backed tenant keys.
// /healthz and /metrics are always allowed through.
func AuthMiddleware(adminKeys map[string]struct{}, s store.SandboxStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("X-Api-Key")
			if key == "" {
				writeError(w, http.StatusUnauthorized, "missing X-Api-Key header")
				return
			}

			if constantTimeContains(adminKeys, key) {
				next.ServeHTTP(w, r.WithContext(withAuthIdentity(r.Context(), authIdentity{Role: roleAdmin})))
				return
			}

			prefix := apiKeyPrefix(key)
			candidates, err := s.ListActiveAPIKeysByPrefix(prefix)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "auth lookup failed")
				return
			}
			wantHash := hashAPIKey(key)
			for _, c := range candidates {
				if subtle.ConstantTimeCompare([]byte(c.KeyHash), []byte(wantHash)) == 1 {
					next.ServeHTTP(w, r.WithContext(withAuthIdentity(r.Context(), authIdentity{
						Role:     roleTenant,
						TenantID: c.TenantID,
					})))
					return
				}
			}

			writeError(w, http.StatusUnauthorized, "invalid API key")
		})
	}
}

// constantTimeContains checks if key exists in the allowed set using constant-time comparison.
func constantTimeContains(allowed map[string]struct{}, key string) bool {
	for k := range allowed {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id, ok := authFromContext(r.Context())
	if !ok || id.Role != roleAdmin {
		writeError(w, http.StatusForbidden, "admin API key required")
		return false
	}
	return true
}

// canAccessSandbox reports whether the caller may see/mutate the sandbox.
// Unknown identity is denied. Admin may access any; tenants only their own.
func canAccessSandbox(r *http.Request, rec *store.SandboxRecord) bool {
	id, ok := authFromContext(r.Context())
	if !ok {
		return false
	}
	if id.Role == roleAdmin {
		return true
	}
	return id.Role == roleTenant && rec.TenantID != "" && rec.TenantID == id.TenantID
}
