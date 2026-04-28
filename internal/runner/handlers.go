package runner

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CreateSandboxRequest is the optional JSON body for POST /sandboxes.
type CreateSandboxRequest struct {
	NetworkPolicy   *manager.NetworkPolicy  `json:"network_policy,omitempty"`
	ResourceLimits  *manager.ResourceLimits `json:"resource_limits,omitempty"`
	DockerfileSteps []string                `json:"dockerfile_steps,omitempty"`
}

// ContainerResponse is the JSON response for container operations.
type ContainerResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	ImageID      string `json:"image_id"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
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

		// Validate dockerfile steps
		for i, step := range req.DockerfileSteps {
			if strings.TrimSpace(step) == "" {
				writeError(w, http.StatusBadRequest, "dockerfile_steps["+strconv.Itoa(i)+"] must be a non-empty string")
				return
			}
		}

		// Get sandbox ID from header (set by API gateway)
		sandboxID := r.Header.Get("X-Sandbox-Id")
		if sandboxID == "" {
			writeError(w, http.StatusBadRequest, "missing X-Sandbox-Id header")
			return
		}

		opts := &manager.CreateOptions{
			NetworkPolicy:   req.NetworkPolicy,
			ResourceLimits:  req.ResourceLimits,
			DockerfileSteps: req.DockerfileSteps,
		}

		containerInfo, err := mgr.CreateContainer(r.Context(), sandboxID, opts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Return container info in sandbox response format
		resp := &ContainerResponse{
			ID:       sandboxID,
			Status:   "running",
			Provider: "delhi",
			ImageID:  containerInfo.ImageTag,
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// GetSandbox handles GET /sandboxes/{id}
func GetSandbox(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		// Try to get container info by container ID (same as sandbox ID for runner)
		containerInfo, err := mgr.GetContainerInfo(r.Context(), id)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		resp := &ContainerResponse{
			ID:       containerInfo.ID,
			Status:   "running",
			Provider: "delhi",
			ImageID:  containerInfo.ImageTag,
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

		// Get container info to get IP
		containerInfo, err := mgr.GetContainerInfo(r.Context(), containerID)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		if err := mgr.DeleteContainer(r.Context(), containerID, containerInfo.IP); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// Placeholder handlers for images - runner doesn't manage image metadata anymore
func ListImages(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []interface{}{})
	}
}

func GetImage(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "image not found")
	}
}

func DeleteImage(mgr ContainerManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "image not found")
	}
}

// isValidID checks that the sandbox ID is a valid UUID.
func isValidID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}
