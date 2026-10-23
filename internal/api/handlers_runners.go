package api

import (
	"log/slog"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
)

type runnerResponse struct {
	ID              string `json:"id"`
	Healthy         bool   `json:"healthy"`
	CapacityTotal   int32  `json:"capacity_total"`
	CapacityUsed    int32  `json:"capacity_used"`
	CapacityStopped int32  `json:"capacity_stopped"`
	LastSeen        int64  `json:"last_seen"`
}

type listRunnersResponse struct {
	Runners []runnerResponse `json:"runners"`
}

func handleListRunners(reg registry.RunnerRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdminOrScaler(w, r) {
			return
		}
		runners, err := reg.List()
		if err != nil {
			slog.ErrorContext(r.Context(), "list runners", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list runners")
			return
		}
		out := listRunnersResponse{Runners: make([]runnerResponse, 0, len(runners))}
		for _, run := range runners {
			out.Runners = append(out.Runners, runnerResponse{
				ID:              run.ID,
				Healthy:         run.Healthy,
				CapacityTotal:   run.CapacityTotal,
				CapacityUsed:    run.CapacityUsed,
				CapacityStopped: run.CapacityStopped,
				LastSeen:        run.LastSeen.Unix(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleRunnerDrain serves the runner drain routes, which are reserved but not
// implemented: placement cannot be stopped on one runner yet.
func handleRunnerDrain() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		writeErrorReason(w, http.StatusNotImplemented, "runner drain is not implemented", reasonNotImplemented)
	}
}
