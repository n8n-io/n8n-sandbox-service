package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/manager"
	"github.com/n8n-io/sandbox-service/internal/store"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CreateSandboxRequest is the optional JSON body for POST /sandboxes.
type CreateSandboxRequest struct {
	NetworkPolicy   *manager.NetworkPolicy  `json:"network_policy,omitempty"`
	ResourceLimits  *manager.ResourceLimits `json:"resource_limits,omitempty"`
	DockerfileSteps []string                `json:"dockerfile_steps,omitempty"`
}

// SandboxManager is the interface that handlers require from the manager.
type SandboxManager interface {
	Create(ctx context.Context, opts *manager.CreateOptions) (*store.SandboxRecord, error)
	Get(ctx context.Context, id string) (*store.SandboxRecord, error)
	List(ctx context.Context) ([]*store.SandboxRecord, error)
	Delete(ctx context.Context, id string) error
	DaemonURL(ctx context.Context, id string) (string, error)
	GetImage(ctx context.Context, id string) (*store.ImageRecord, error)
	ListImages(ctx context.Context) ([]*store.ImageRecord, error)
	DeleteImage(ctx context.Context, id string) error
}

// SandboxResponse is the JSON body for sandbox creation/get responses.
type SandboxResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	ImageID      string `json:"image_id"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
}

func sandboxResponseFrom(rec *store.SandboxRecord) *SandboxResponse {
	return &SandboxResponse{
		ID:           rec.ID,
		Status:       rec.Status,
		Provider:     "delhi",
		ImageID:      rec.ImageID,
		CreatedAt:    rec.CreatedAt,
		LastActiveAt: rec.LastActiveAt,
	}
}

// ListSandboxes handles GET /sandboxes
func ListSandboxes(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, err := mgr.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]*SandboxResponse, len(records))
		for i, rec := range records {
			resp[i] = sandboxResponseFrom(rec)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// CreateSandbox handles POST /sandboxes
func CreateSandbox(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest

		// Parse optional JSON body (empty body is fine for backward compat)
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

		opts := &manager.CreateOptions{
			NetworkPolicy:   req.NetworkPolicy,
			ResourceLimits:  req.ResourceLimits,
			DockerfileSteps: req.DockerfileSteps,
		}
		for i, step := range opts.DockerfileSteps {
			if strings.TrimSpace(step) == "" {
				writeError(w, http.StatusBadRequest, "dockerfile_steps["+strconv.Itoa(i)+"] must be a non-empty string")
				return
			}
		}

		rec, err := mgr.Create(r.Context(), opts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, sandboxResponseFrom(rec))
	}
}

// GetSandbox handles GET /sandboxes/{id}
func GetSandbox(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		rec, err := mgr.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		if rec == nil {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeJSON(w, http.StatusOK, sandboxResponseFrom(rec))
	}
}

// DeleteSandbox handles DELETE /sandboxes/{id}
func DeleteSandbox(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		if err := mgr.Delete(r.Context(), id); err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// isValidID checks that the sandbox ID is a valid UUID.
func isValidID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}
