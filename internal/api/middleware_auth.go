package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

type authRole string

const (
	roleAdmin authRole = "admin"
	// roleProvisioner: tenant create and delete only. See
	// docs/security-model.md, "Provisioner keys".
	roleProvisioner authRole = "provisioner"
	roleTenant      authRole = "tenant"
)

// provisionerPathPrefix confines provisioner keys; the handlers under it other
// than tenant create and delete require admin.
const provisionerPathPrefix = "/admin/tenants"

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

// AuthMiddleware checks X-Api-Key against env admin keys, then env provisioner
// keys, then DB-backed tenant keys. /healthz and /metrics are always allowed
// through.
func AuthMiddleware(adminKeys, provisionerKeys map[string]struct{}, s store.SandboxStore) func(http.Handler) http.Handler {
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

			if constantTimeContains(provisionerKeys, key) {
				if !strings.HasPrefix(r.URL.Path, provisionerPathPrefix) {
					writeError(w, http.StatusForbidden, "provisioner API key may only manage tenants")
					return
				}
				next.ServeHTTP(w, r.WithContext(withAuthIdentity(r.Context(), authIdentity{Role: roleProvisioner})))
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
					obs.FieldsFrom(r.Context()).Add("tenant_id", c.TenantID)
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

// requireAdminOrProvisioner gates the two tenant routes a provisioner may call.
// Returns the caller's role so the handler can apply provisioner-only limits.
func requireAdminOrProvisioner(w http.ResponseWriter, r *http.Request) (authRole, bool) {
	id, ok := authFromContext(r.Context())
	if !ok || (id.Role != roleAdmin && id.Role != roleProvisioner) {
		writeError(w, http.StatusForbidden, "admin or provisioner API key required")
		return "", false
	}
	return id.Role, true
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
	return id.Role == roleTenant && !store.IsAdminTenantID(rec.TenantID) && rec.TenantID == id.TenantID
}
