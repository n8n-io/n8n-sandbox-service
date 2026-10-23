package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
)

type runnerResponse struct {
	ID              string `json:"id"`
	Healthy         bool   `json:"healthy"`
	CapacityTotal   int32  `json:"capacity_total"`
	CapacityUsed    int32  `json:"capacity_used"`
	CapacityStopped int32  `json:"capacity_stopped"`
	Cordoned        bool   `json:"cordoned"`
	// Templates the runner serves, by name; a runner that lists none serves "default".
	Templates []string `json:"templates"`
	LastSeen  int64    `json:"last_seen"`
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
				Cordoned:        run.Cordoned,
				Templates:       servedTemplates(run),
				LastSeen:        run.LastSeen.Unix(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func servedTemplates(run registry.Runner) []string {
	if len(run.Templates) == 0 {
		return []string{registry.DefaultTemplate}
	}
	names := make([]string, len(run.Templates))
	for i, t := range run.Templates {
		names[i] = t.Name
	}
	return names
}

// drainState is the body of the runner drain routes.
type drainState struct {
	RunnerID         string `json:"runner_id"`
	Draining         bool   `json:"draining"`
	Drained          bool   `json:"drained"`
	ActiveExecutions int32  `json:"active_executions"`
	OpenStreams      int32  `json:"open_streams"`
	CapacityUsed     int32  `json:"capacity_used"`
	CapacityStopped  int32  `json:"capacity_stopped"`
}

// drainCallBudget bounds each runner call a drain route makes.
const drainCallBudget = 10 * time.Second

// handleRunnerDrain serves GET (state), POST (start) and DELETE (stop) on
// /admin/runners/{id}/drain. Draining is the runner's cordon: it refuses
// creates, and placement skips it. Drained means draining with nothing left
// running, so the runner can be restarted without cutting a run short.
func handleRunnerDrain(reg registry.RunnerRegistry, cfg *config.APIConfig) http.HandlerFunc {
	tlsCfg := runnerControlTLS(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id := r.PathValue("id")
		run, ok := reg.Get(id)
		if !ok || strings.TrimSpace(run.ControlGRPCAddr) == "" {
			writeError(w, http.StatusNotFound, "runner not found")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), drainCallBudget)
		defer cancel()
		if r.Method != http.MethodGet {
			if _, err := runnerctl.SetCordon(ctx, run.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, r.Method == http.MethodPost); err != nil {
				writeRunnerUnavailable(w, r, id, err)
				return
			}
			slog.InfoContext(r.Context(), "runner drain changed", "runner_id", id, "draining", r.Method == http.MethodPost)
		}
		act, err := runnerctl.GetActivity(ctx, run.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg)
		if err != nil {
			writeRunnerUnavailable(w, r, id, err)
			return
		}
		writeJSON(w, http.StatusOK, drainState{
			RunnerID:         id,
			Draining:         act.GetCordoned(),
			Drained:          act.GetCordoned() && act.GetActiveExecutions() == 0 && act.GetOpenStreams() == 0,
			ActiveExecutions: act.GetActiveExecutions(),
			OpenStreams:      act.GetOpenStreams(),
			CapacityUsed:     act.GetCapacityUsed(),
			CapacityStopped:  act.GetCapacityStopped(),
		})
	}
}

// writeRunnerUnavailable answers a runner call that failed. The runner's own
// message is logged, not relayed.
func writeRunnerUnavailable(w http.ResponseWriter, r *http.Request, runnerID string, err error) {
	slog.WarnContext(r.Context(), "runner drain call failed", "runner_id", runnerID, "err", err)
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "runner unavailable")
}
