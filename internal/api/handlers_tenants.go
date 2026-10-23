package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

type createTenantRequest struct {
	Name         string `json:"name"`
	ExternalRef  string `json:"external_ref"`
	MaxSandboxes *int   `json:"max_sandboxes"`
	CreateKey    *bool  `json:"create_key"` // default true
	// MaxIdleWindow caps the tenant's per-sandbox idle windows, in seconds.
	MaxIdleWindow *int64 `json:"max_idle_window"`
}

// updateTenantRequest holds the limits PATCH may change; every field is optional
// but at least one must be set. max_idle_window is raw so that null, which
// clears it, differs from leaving it out.
type updateTenantRequest struct {
	MaxSandboxes  *int            `json:"max_sandboxes"`
	MaxIdleWindow json.RawMessage `json:"max_idle_window"`
}

type tenantResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ExternalRef  string `json:"external_ref"`
	MaxSandboxes int    `json:"max_sandboxes"`
	CreatedAt    int64  `json:"created_at"`
	// MaxIdleWindow is present when set; otherwise the server's cap applies.
	MaxIdleWindow int64 `json:"max_idle_window,omitempty"`
}

type apiKeyResponse struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	RevokedAt int64  `json:"revoked_at,omitempty"`
	APIKey    string `json:"api_key,omitempty"` // plaintext only on create
}

type createTenantResponse struct {
	Tenant tenantResponse  `json:"tenant"`
	Key    *apiKeyResponse `json:"key,omitempty"`
}

func tenantToResponse(t *store.Tenant) tenantResponse {
	return tenantResponse{
		ID:            t.ID,
		Name:          t.Name,
		ExternalRef:   t.ExternalRef,
		MaxSandboxes:  t.MaxSandboxes,
		CreatedAt:     t.CreatedAt,
		MaxIdleWindow: t.MaxIdleWindow,
	}
}

// maxIdleWindowError returns why a tenant's idle window cap cannot be n
// seconds, or "" when it can: it narrows the server's cap, never widens it.
func maxIdleWindowError(n int64, cfg *config.APIConfig) string {
	limit := int64(cfg.MaxIdleWindow / time.Second)
	if limit == 0 {
		return "max_idle_window is not allowed: SANDBOX_API_MAX_IDLE_WINDOW is 0"
	}
	if minimum := int64(config.MinIdleWindow / time.Second); n < minimum || n > limit {
		return "max_idle_window must be between " + strconv.FormatInt(minimum, 10) + " and " + strconv.FormatInt(limit, 10) + " seconds"
	}
	return ""
}

func apiKeyToResponse(k *store.APIKey, plaintext string) apiKeyResponse {
	resp := apiKeyResponse{
		ID:        k.ID,
		TenantID:  k.TenantID,
		Prefix:    k.Prefix,
		CreatedAt: k.CreatedAt,
		RevokedAt: k.RevokedAt,
	}
	if plaintext != "" {
		resp.APIKey = plaintext
	}
	return resp
}

// maxSandboxesError returns why the caller may not set n, or "" when it may.
// Only admin keys may set unlimited (0) or more than the default.
func maxSandboxesError(role authRole, n int, cfg *config.APIConfig) string {
	if n < 0 || n > math.MaxInt32 {
		return "max_sandboxes must be between 0 and 2147483647"
	}
	if role != roleAdmin && (n < 1 || n > cfg.DefaultMaxSandboxes) {
		return "max_sandboxes must be between 1 and " + strconv.Itoa(cfg.DefaultMaxSandboxes) + " for a provisioner key"
	}
	return ""
}

func handleListTenants(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		tenants, err := s.ListTenants()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]tenantResponse, 0, len(tenants))
		for _, t := range tenants {
			resp = append(resp, tenantToResponse(t))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleCreateTenant(s store.SandboxStore, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := requireAdminOrProvisioner(w, r)
		if !ok {
			return
		}
		var req createTenantRequest
		if r.Body != nil && r.ContentLength != 0 {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			if err := dec.Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if _, err := dec.Token(); err != io.EOF {
				writeError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
		}
		maxSandboxes := cfg.DefaultMaxSandboxes
		if req.MaxSandboxes != nil {
			if msg := maxSandboxesError(role, *req.MaxSandboxes, cfg); msg != "" {
				writeError(w, http.StatusBadRequest, msg)
				return
			}
			maxSandboxes = *req.MaxSandboxes
		}
		var maxIdleWindow int64
		if req.MaxIdleWindow != nil {
			if msg := maxIdleWindowError(*req.MaxIdleWindow, cfg); msg != "" {
				writeError(w, http.StatusBadRequest, msg)
				return
			}
			maxIdleWindow = *req.MaxIdleWindow
		}
		createKey := true
		if req.CreateKey != nil {
			createKey = *req.CreateKey
		}

		now := time.Now().Unix()
		t := &store.Tenant{
			ID:            uuid.New().String(),
			Name:          req.Name,
			ExternalRef:   req.ExternalRef,
			MaxSandboxes:  maxSandboxes,
			CreatedAt:     now,
			MaxIdleWindow: maxIdleWindow,
		}

		resp := createTenantResponse{Tenant: tenantToResponse(t)}
		if createKey {
			k, plaintext, err := newAPIKey(t.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to create api key: "+err.Error())
				return
			}
			if err := s.CreateTenantWithAPIKey(t, k); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to create tenant: "+err.Error())
				return
			}
			keyResp := apiKeyToResponse(k, plaintext)
			resp.Key = &keyResp
		} else if err := s.CreateTenant(t); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create tenant: "+err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

func handleGetTenant(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid tenant id")
			return
		}
		t, err := s.GetTenant(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		writeJSON(w, http.StatusOK, tenantToResponse(t))
	}
}

// handleUpdateTenant changes a tenant's limits and never touches its sandboxes:
// a tenant above a lowered max_sandboxes keeps them and cannot create more until
// it is below the limit again. Provisioner keys get the same bounds as on create.
func handleUpdateTenant(s store.SandboxStore, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := requireAdminOrProvisioner(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid tenant id")
			return
		}
		var req updateTenantRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.MaxSandboxes == nil && req.MaxIdleWindow == nil {
			writeError(w, http.StatusBadRequest, "max_sandboxes or max_idle_window is required")
			return
		}
		if req.MaxSandboxes != nil {
			if msg := maxSandboxesError(role, *req.MaxSandboxes, cfg); msg != "" {
				writeError(w, http.StatusBadRequest, msg)
				return
			}
		}
		var maxIdleWindow *int64
		if req.MaxIdleWindow != nil {
			var n *int64
			if err := json.Unmarshal(req.MaxIdleWindow, &n); err != nil {
				writeError(w, http.StatusBadRequest, "max_idle_window must be an integer or null")
				return
			}
			if n == nil {
				n = new(int64) // null: back to the server's cap
			} else if msg := maxIdleWindowError(*n, cfg); msg != "" {
				writeError(w, http.StatusBadRequest, msg)
				return
			}
			maxIdleWindow = n
		}

		t, err := s.GetTenant(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		logArgs := []any{"tenant_id", id, "role", string(role)}
		if req.MaxSandboxes != nil {
			if err := s.UpdateTenantLimits(id, *req.MaxSandboxes); err != nil {
				writeTenantUpdateError(w, err)
				return
			}
			logArgs = append(logArgs, "old_max_sandboxes", t.MaxSandboxes, "max_sandboxes", *req.MaxSandboxes)
			t.MaxSandboxes = *req.MaxSandboxes
		}
		if maxIdleWindow != nil {
			if err := s.SetTenantMaxIdleWindow(id, *maxIdleWindow); err != nil {
				writeTenantUpdateError(w, err)
				return
			}
			logArgs = append(logArgs, "old_max_idle_window", t.MaxIdleWindow, "max_idle_window", *maxIdleWindow)
			t.MaxIdleWindow = *maxIdleWindow
		}
		slog.InfoContext(r.Context(), "tenant limits updated", logArgs...)
		writeJSON(w, http.StatusOK, tenantToResponse(t))
	}
}

func writeTenantUpdateError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrTenantNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// handleDeleteTenant is safe for provisioners because of the 409 below.
func handleDeleteTenant(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdminOrProvisioner(w, r); !ok {
			return
		}
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid tenant id")
			return
		}
		t, err := s.GetTenant(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := s.DeleteTenant(id); err != nil {
			if errors.Is(err, store.ErrTenantHasSandboxes) {
				writeError(w, http.StatusConflict, "tenant has sandboxes; delete them first")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleListTenantKeys(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		tenantID := r.PathValue("id")
		if !isValidUUID(tenantID) {
			writeError(w, http.StatusBadRequest, "invalid tenant id")
			return
		}
		t, err := s.GetTenant(tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		keys, err := s.ListAPIKeysByTenant(tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]apiKeyResponse, 0, len(keys))
		for _, k := range keys {
			resp = append(resp, apiKeyToResponse(k, ""))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleCreateTenantKey(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		tenantID := r.PathValue("id")
		if !isValidUUID(tenantID) {
			writeError(w, http.StatusBadRequest, "invalid tenant id")
			return
		}
		t, err := s.GetTenant(tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		keyResp, err := mintAPIKey(s, tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create api key: "+err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, keyResp)
	}
}

func handleRevokeTenantKey(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		tenantID := r.PathValue("id")
		keyID := r.PathValue("keyId")
		if !isValidUUID(tenantID) || !isValidUUID(keyID) {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		k, err := s.GetAPIKey(keyID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if k == nil || k.TenantID != tenantID {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		if err := s.RevokeAPIKey(keyID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func newAPIKey(tenantID string) (*store.APIKey, string, error) {
	prefixBytes := make([]byte, 4)
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(prefixBytes); err != nil {
		return nil, "", err
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", err
	}
	prefix := hex.EncodeToString(prefixBytes)
	plaintext := "sbk_" + prefix + "_" + hex.EncodeToString(secretBytes)
	k := &store.APIKey{
		ID:        uuid.New().String(),
		TenantID:  tenantID,
		KeyHash:   hashAPIKey(plaintext),
		Prefix:    prefix,
		CreatedAt: time.Now().Unix(),
	}
	return k, plaintext, nil
}

func mintAPIKey(s store.SandboxStore, tenantID string) (*apiKeyResponse, error) {
	k, plaintext, err := newAPIKey(tenantID)
	if err != nil {
		return nil, err
	}
	if err := s.CreateAPIKey(k); err != nil {
		return nil, err
	}
	resp := apiKeyToResponse(k, plaintext)
	return &resp, nil
}
