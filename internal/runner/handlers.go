package runner

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CreateSandboxRequest is the optional JSON body for POST /sandboxes.
type CreateSandboxRequest struct{}

// ContainerResponse is the JSON response for container operations.
type ContainerResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
	IP           string `json:"ip,omitempty"`
}

// ListSandboxes handles GET /sandboxes - returns empty for stateless runner
func ListSandboxes(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Stateless runner has no list of sandboxes
		writeJSON(w, http.StatusOK, []ContainerResponse{})
	}
}

// CreateSandbox handles POST /sandboxes
func CreateSandbox(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest

		// Parse optional JSON body
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "failed to read request body")
				return
			}
			if len(body) > 0 {
				if err := json.Unmarshal(body, &req); err != nil {
					writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
					return
				}
			}
		}

		// Get sandbox ID from header (set by API gateway)
		sandboxID := r.Header.Get("X-Sandbox-Id")
		if sandboxID == "" {
			writeError(w, http.StatusBadRequest, "missing X-Sandbox-Id header")
			return
		}
		if !isValidID(sandboxID) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		opts := &manager.CreateOptions{}

		info, err := mgr.CreateContainer(r.Context(), sandboxID, opts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		resp := &ContainerResponse{
			ID:     sandboxID,
			Status: "running",
		}
		if info != nil {
			resp.IP = info.IP
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// GetSandbox handles GET /sandboxes/{id}
func GetSandbox(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sandboxID := r.PathValue("id")
		if !isValidID(sandboxID) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		if _, err := mgr.FindContainerIDByLabel(r.Context(), sandboxID); err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		resp := &ContainerResponse{
			ID:     sandboxID,
			Status: "running",
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// DeleteSandbox handles DELETE /sandboxes/{id}
func DeleteSandbox(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sandboxID := r.PathValue("id")
		if !isValidID(sandboxID) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		// Find container ID by sandbox ID using labels
		containerID, err := mgr.FindContainerIDByLabel(r.Context(), sandboxID)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		if err := mgr.DeleteContainer(r.Context(), containerID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// isValidID checks that the sandbox ID is a valid UUID.
func isValidID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}
