package daemon

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
)

// mountVolumes serves POST /volumes/mount. In a daemon started as root, the
// first request mounts its volume drives and then drops root, whatever the
// outcome; later requests get 409. A daemon that did not start as root answers
// 501.
func (h *Handler) mountVolumes(w http.ResponseWriter, r *http.Request) {
	if h.volumes == nil {
		writeError(w, http.StatusNotImplemented, "volumes can only be mounted by a daemon started as root")
		return
	}
	if !h.volumesClaimed.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "volumes can only be mounted by the first request")
		return
	}
	// Also on a panic: a daemon that keeps root after this request exits.
	droppedRoot := false
	defer func() {
		if !droppedRoot {
			h.exit(1)
		}
	}()

	resp := errorResponse{Code: http.StatusNoContent}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, volumedrives.MaxRequestBytes))
	var volumes []volumedrives.Volume
	if err == nil {
		volumes, err = volumedrives.ParseRequest(body)
	}
	if err != nil {
		resp = errorResponse{Code: http.StatusBadRequest, Error: "invalid request body: " + err.Error()}
	} else if err := h.volumes.MountAll(volumes); err != nil {
		resp = errorResponse{Code: http.StatusInternalServerError, Error: "mount volumes: " + err.Error(), Reason: volumedrives.Reason(err)}
	}

	if err := h.dropRoot(); err != nil {
		slog.Error("drop root after volume mounts", "err", err)
		return
	}
	droppedRoot = true

	if resp.Code != http.StatusNoContent {
		slog.Warn("mount volumes", "status", resp.Code, "reason", resp.Reason, "err", resp.Error)
		writeJSON(w, resp.Code, resp)
		return
	}
	slog.Info("volumes mounted", "count", len(volumes))
	w.WriteHeader(http.StatusNoContent)
}
