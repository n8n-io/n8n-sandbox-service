// Package api implements the API gateway: authentication, tenant scoping, sandbox
// CRUD, proxying of exec and file requests to runners, and the idle sweeper.
package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// NewGatewayRouter creates the public API gateway that manages state and
// coordinates with registered runner services. If rec is enabled, HTTPMiddleware
// wraps the request chain, and its /metrics handler is mounted here unless
// cfg gives metrics a listener of their own.
func NewGatewayRouter(s store.SandboxStore, cfg *config.APIConfig, reg registry.RunnerRegistry, rec *metrics.APIRecorder) (http.Handler, error) {
	runnerTransport, err := newRunnerTransport(cfg)
	if err != nil {
		return nil, err
	}
	sandboxProxy := sandboxProxyHandler(s, cfg, runnerTransport)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// A dedicated metrics port is served by NewMetricsRouter instead.
	if rec.Enabled() && cfg.MetricsOnMainListener() {
		mux.Handle("GET /metrics", metrics.Handler(rec.Registry()))
	}

	limitTenant := tenantLimit(limits.NewKeyedLimiter(cfg.MaxInflightPerTenant), rec)
	// Execution DELETEs have a limit of their own, so a tenant can cancel work
	// while its other requests fill theirs. Turning the tenant limit off turns
	// this one off too.
	maxDeletes := 0
	if cfg.MaxInflightPerTenant > 0 {
		maxDeletes = maxExecutionDeletesPerTenant
	}
	limitTenantDeletes := tenantLimit(limits.NewKeyedLimiter(maxDeletes), rec)
	mux.HandleFunc("GET /sandboxes", handleListSandboxes(s))
	// A create holds its slot until the runner is done, even after the client
	// has gone, so a burst of creates is bounded like any other request.
	mux.HandleFunc("POST /sandboxes", limitTenant(handleCreateSandbox(s, reg, cfg, rec)))
	mux.HandleFunc("GET /sandboxes/{id}", handleGetSandbox(s, cfg))
	mux.HandleFunc("DELETE /sandboxes/{id}", handleDeleteSandbox(s, cfg, rec))

	mux.HandleFunc("GET /admin/tenants", handleListTenants(s))
	mux.HandleFunc("POST /admin/tenants", handleCreateTenant(s, cfg))
	mux.HandleFunc("GET /admin/tenants/{id}", handleGetTenant(s))
	mux.HandleFunc("DELETE /admin/tenants/{id}", handleDeleteTenant(s))
	mux.HandleFunc("GET /admin/tenants/{id}/keys", handleListTenantKeys(s))
	mux.HandleFunc("POST /admin/tenants/{id}/keys", handleCreateTenantKey(s))
	mux.HandleFunc("DELETE /admin/tenants/{id}/keys/{keyId}", handleRevokeTenantKey(s))

	mux.HandleFunc("POST /sandboxes/{id}/executions", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("GET /sandboxes/{id}/executions/{exec_id}", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("DELETE /sandboxes/{id}/executions/{exec_id}", limitTenantDeletes(sandboxProxy(false)))
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("POST /sandboxes/{id}/files/move", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("GET /sandboxes/{id}/files", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("GET /sandboxes/{id}/files/content", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("PUT /sandboxes/{id}/files", limitTenant(sandboxProxy(true)))
	mux.HandleFunc("POST /sandboxes/{id}/files", limitTenant(sandboxProxy(true)))
	mux.HandleFunc("DELETE /sandboxes/{id}/files", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", limitTenant(sandboxProxy(false)))
	mux.HandleFunc("GET /sandboxes/{id}/stat", limitTenant(sandboxProxy(false)))

	var handler http.Handler = mux
	if rec.Enabled() {
		handler = metrics.HTTPMiddleware(rec)(handler)
	}
	handler = AuthMiddleware(cfg.APIKeys, cfg.ProvisionerKeys, s)(handler)
	handler = publicPaths.RejectOtherMethods(handler)
	handler = LoggingMiddleware(handler)
	if cfg.EnableCORS {
		handler = CORSMiddleware(handler)
	}
	handler = RecoveryMiddleware(handler)
	return handler, nil
}
