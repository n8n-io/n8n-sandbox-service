package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
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
}

type tenantResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ExternalRef  string `json:"external_ref"`
	MaxSandboxes int    `json:"max_sandboxes"`
	CreatedAt    int64  `json:"created_at"`
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
		ID:           t.ID,
		Name:         t.Name,
		ExternalRef:  t.ExternalRef,
		MaxSandboxes: t.MaxSandboxes,
		CreatedAt:    t.CreatedAt,
	}
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
		if !requireAdmin(w, r) {
			return
		}
		var req createTenantRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
		}
		maxSandboxes := cfg.DefaultMaxSandboxes
		if req.MaxSandboxes != nil {
			if *req.MaxSandboxes < 0 {
				writeError(w, http.StatusBadRequest, "max_sandboxes must be >= 0")
				return
			}
			maxSandboxes = *req.MaxSandboxes
		}
		createKey := true
		if req.CreateKey != nil {
			createKey = *req.CreateKey
		}

		now := time.Now().Unix()
		t := &store.Tenant{
			ID:           uuid.New().String(),
			Name:         req.Name,
			ExternalRef:  req.ExternalRef,
			MaxSandboxes: maxSandboxes,
			CreatedAt:    now,
		}
		if err := s.CreateTenant(t); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create tenant: "+err.Error())
			return
		}

		resp := createTenantResponse{Tenant: tenantToResponse(t)}
		if createKey {
			keyResp, err := mintAPIKey(s, t.ID)
			if err != nil {
				_ = s.DeleteTenant(t.ID)
				writeError(w, http.StatusInternalServerError, "failed to create api key: "+err.Error())
				return
			}
			resp.Key = keyResp
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

func handleDeleteTenant(s store.SandboxStore) http.HandlerFunc {
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
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := s.DeleteTenant(id); err != nil {
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

func mintAPIKey(s store.SandboxStore, tenantID string) (*apiKeyResponse, error) {
	prefixBytes := make([]byte, 4)
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(prefixBytes); err != nil {
		return nil, err
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, err
	}
	prefix := hex.EncodeToString(prefixBytes)
	plaintext := "sbk_" + prefix + "_" + hex.EncodeToString(secretBytes)
	now := time.Now().Unix()
	k := &store.APIKey{
		ID:        uuid.New().String(),
		TenantID:  tenantID,
		KeyHash:   hashAPIKey(plaintext),
		Prefix:    prefix,
		CreatedAt: now,
	}
	if err := s.CreateAPIKey(k); err != nil {
		return nil, err
	}
	resp := apiKeyToResponse(k, plaintext)
	return &resp, nil
}
