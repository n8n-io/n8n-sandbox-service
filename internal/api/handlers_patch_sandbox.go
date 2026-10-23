package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// patchSandboxRequest holds what PATCH /sandboxes/{id} may change. Fields are
// raw so that null, which clears a window back to the global one, differs
// from leaving it out.
type patchSandboxRequest struct {
	IdleStopAfter   json.RawMessage `json:"idle_stop_after"`
	IdleDeleteAfter json.RawMessage `json:"idle_delete_after"`
}

// windowPatch applies one raw field to cur: absent keeps it, null clears it.
func windowPatch(field string, raw json.RawMessage, cur int64, limit int64) (int64, error) {
	if raw == nil {
		return cur, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, errors.New(field + " must be an integer or null")
	}
	if err := checkIdleWindow(field, n, limit); err != nil {
		return 0, err
	}
	return n, nil
}

// handlePatchSandbox changes a sandbox's idle windows. The new windows apply
// from the next sweep; a stopped sandbox stays stopped.
func handlePatchSandbox(s store.SandboxStore, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		var req patchSandboxRequest
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
		if req.IdleStopAfter == nil && req.IdleDeleteAfter == nil {
			writeError(w, http.StatusBadRequest, "idle_stop_after or idle_delete_after is required")
			return
		}

		unlock, err := s.LockSandbox(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer unlock()
		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil || !canAccessSandbox(r, rec) || isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		// The owner's cap, not the caller's: an admin changing a tenant's
		// sandbox stays within what the tenant could have set.
		limit, err := idleWindowLimit(s, cfg, rec.TenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		stop, err := windowPatch("idle_stop_after", req.IdleStopAfter, rec.IdleStopAfter, limit)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		del, err := windowPatch("idle_delete_after", req.IdleDeleteAfter, rec.IdleDeleteAfter, limit)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if stop > 0 && del > 0 && stop > del {
			writeError(w, http.StatusBadRequest, "idle_stop_after must not exceed idle_delete_after")
			return
		}
		if err := s.UpdateIdleWindows(id, stop, del); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.InfoContext(r.Context(), "sandbox idle windows updated", "sandbox_id", id,
			"idle_stop_after", stop, "idle_delete_after", del)
		rec.IdleStopAfter, rec.IdleDeleteAfter = stop, del
		writeJSON(w, http.StatusOK, sandboxResponse(rec))
	}
}
