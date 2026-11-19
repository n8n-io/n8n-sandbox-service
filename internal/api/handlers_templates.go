package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
)

type templateResponse struct {
	Name     string `json:"name"`
	VCPUs    int32  `json:"vcpus"`
	MemoryMB int64  `json:"memory_mb"`
	DiskMB   int64  `json:"disk_mb"`
	// Runners counts the healthy, undrained runners serving it now.
	Runners int `json:"runners"`
}

type listTemplatesResponse struct {
	Templates []templateResponse `json:"templates"`
}

// handleListTemplates lists the catalog. Sizes come from a runner serving the
// template, so they are 0 for one no live runner serves; creates with it then
// answer 503 no_capacity.
func handleListTemplates(reg registry.RunnerRegistry, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := authFromContext(r.Context())
		if !ok || (id.Role != roleAdmin && id.Role != roleTenant) {
			writeError(w, http.StatusForbidden, "template listing requires an admin or tenant API key")
			return
		}
		runners, err := reg.List()
		if err != nil {
			slog.ErrorContext(r.Context(), "list runners", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list runners")
			return
		}
		catalog := cfg.Templates
		if len(catalog) == 0 {
			catalog = []string{registry.DefaultTemplate}
		}
		now := time.Now()
		out := listTemplatesResponse{Templates: make([]templateResponse, 0, len(catalog))}
		for _, name := range catalog {
			t := templateResponse{Name: name}
			for i := range runners {
				run := &runners[i]
				if !run.Serves(name) {
					continue
				}
				if t.VCPUs == 0 && t.MemoryMB == 0 && t.DiskMB == 0 {
					for _, rt := range run.Templates {
						if rt.Name == name {
							t.VCPUs, t.MemoryMB, t.DiskMB = rt.VCPUs, rt.MemoryMB, rt.DiskMB
						}
					}
				}
				if run.Healthy && !run.Cordoned && now.Sub(run.LastSeen) <= cfg.HeartbeatGrace {
					t.Runners++
				}
			}
			out.Templates = append(out.Templates, t)
		}
		writeJSON(w, http.StatusOK, out)
	}
}
