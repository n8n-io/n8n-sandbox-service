package api

import (
	"errors"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/manager"
)

type daemonURLResponse struct {
	DaemonURL string `json:"daemon_url"`
}

// GetDaemonURL handles GET /internal/sandboxes/{id}/daemon-url for API->runner proxying.
func GetDaemonURL(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		daemonURL, err := mgr.DaemonURL(r.Context(), id)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		writeJSON(w, http.StatusOK, &daemonURLResponse{DaemonURL: daemonURL})
	}
}
