package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
)

// mountVolumes serves POST /volumes/mount. In a daemon started as root, the
// first request mounts each of its volume drives on its own and then drops
// root, whatever the outcome; later requests get 409. A daemon that did not
// start as root answers 501.
//
// An empty list gets 204, which older runners expect. A list with volumes gets
// 200 and a volumedrives.Answer: each drive with its outcome and nothing else
// from the guest. An invalid request mounts nothing and gets 400.
func (h *Handler) mountVolumes(w http.ResponseWriter, r *http.Request) {
	if h.volumes == nil {
		writeError(w, http.StatusNotImplemented, "volumes can only be mounted by a daemon started as root")
		return
	}
	if !h.volumesClaimed.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "volumes can only be mounted by the first request")
		return
	}
	// Also on a panic: a daemon that keeps root after this request exits. It logs
	// the panic itself, since exiting ends the process before net/http could.
	droppedRoot := false
	defer func() {
		if droppedRoot {
			return
		}
		defer h.exit(1)
		if p := recover(); p != nil {
			slog.Error("volume mount request panicked", "panic", p, "stack", string(debug.Stack()))
		}
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, volumedrives.MaxRequestBytes))
	var volumes []volumedrives.Volume
	if err == nil {
		volumes, err = volumedrives.ParseRequest(body)
	}
	var invalid string
	var outcomes []volumedrives.Outcome
	var mountErr error
	if err != nil {
		invalid = "invalid request body: " + err.Error()
	} else {
		outcomes, mountErr = h.volumes.MountEach(volumes)
	}

	if err := h.dropRoot(); err != nil {
		slog.Error("drop root after volume mounts", "err", err, "request_err", invalid, "mount_err", mountErr)
		return
	}
	droppedRoot = true

	if invalid != "" {
		slog.Warn("mount volumes", "status", http.StatusBadRequest, "err", invalid)
		writeError(w, http.StatusBadRequest, invalid)
		return
	}
	if mountErr != nil {
		slog.Warn("mount volumes", "err", mountErr)
	}
	if len(volumes) == 0 {
		slog.Info("volumes mounted", "count", 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	mounted := 0
	for _, outcome := range outcomes {
		if outcome.Mounted {
			mounted++
		}
	}
	slog.Info("volumes mounted", "count", mounted, "failed", len(outcomes)-mounted)
	writeJSON(w, http.StatusOK, volumedrives.Answer{Volumes: outcomes})
}
