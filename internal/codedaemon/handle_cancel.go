// This file handles DELETE /tasks/{taskId}.

package codedaemon

import "net/http"

// handleCancel reports whether a cancellation was requested, not whether it succeeded.
func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !d.supervisor.requestCancel(r.PathValue("taskId")) {
		writeTaskRejection(w, http.StatusNotFound, "not_found", "task not running")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
