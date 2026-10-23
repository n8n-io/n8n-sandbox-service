package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

const runnerCordonTimeout = 10 * time.Second

type runnerResponse struct {
	ID              string `json:"id"`
	Healthy         bool   `json:"healthy"`
	Cordoned        bool   `json:"cordoned"`
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
				Cordoned:        run.Cordoned,
				CapacityTotal:   run.CapacityTotal,
				CapacityUsed:    run.CapacityUsed,
				CapacityStopped: run.CapacityStopped,
				LastSeen:        run.LastSeen.Unix(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type runnerCordonResponse struct {
	ID       string `json:"id"`
	Cordoned bool   `json:"cordoned"`
}

// handleSetRunnerCordon serves PUT (cordoned) and DELETE (not cordoned)
// /admin/runners/{id}/cordon. The runner holds the state; the registry learns
// it from the heartbeat the runner sends on the change.
func handleSetRunnerCordon(reg registry.RunnerRegistry, cfg *config.APIConfig, cordoned bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdminOrScaler(w, r) {
			return
		}
		id := r.PathValue("id")
		run, ok := reg.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "runner not found")
			return
		}
		fields := obs.FieldsFrom(r.Context())
		fields.Add("runner_id", id)
		if caller, ok := authFromContext(r.Context()); ok {
			fields.Add("auth_role", string(caller.Role))
		}
		// The Postgres registry keeps rows for runners that are long gone.
		if time.Since(run.LastSeen) > cfg.HeartbeatGrace {
			writeError(w, http.StatusServiceUnavailable, "runner unavailable")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), runnerCordonTimeout)
		defer cancel()
		got, err := runnerctl.SetCordon(ctx, run.ControlGRPCAddr, cfg.RunnerAPIKey, runnerControlTLS(cfg), cordoned)
		if err != nil {
			slog.WarnContext(r.Context(), "set runner cordon", "runner_id", id, "cordoned", cordoned, "error", err)
			switch status.Code(err) {
			case codes.Unavailable, codes.DeadlineExceeded:
				writeError(w, http.StatusServiceUnavailable, "runner unavailable")
			default:
				writeError(w, http.StatusBadGateway, "runner did not apply the cordon")
			}
			return
		}
		fields.Add("cordoned", got)
		writeJSON(w, http.StatusOK, runnerCordonResponse{ID: id, Cordoned: got})
	}
}
