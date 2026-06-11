package runner

import (
	"errors"
	"net/http"
	"regexp"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ContainerResponse is the JSON response for container operations.
type ContainerResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
	IP           string `json:"ip,omitempty"`
}

// GetSandbox handles GET /sandboxes/{id}
func GetSandbox(rt runnerruntime.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sandboxID := r.PathValue("id")
		if !isValidID(sandboxID) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		if _, err := rt.GetSandboxInfo(r.Context(), sandboxID); err != nil {
			if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
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

// isValidID checks that the sandbox ID is a valid UUID.
func isValidID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}
