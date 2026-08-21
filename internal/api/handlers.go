package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

func runnerProxyForPick(w http.ResponseWriter, r *http.Request, limitBody bool, cfg *config.APIConfig, pick func() (*registry.Runner, error)) bool {
	run, err := pick()
	if err != nil {
		if errors.Is(err, registry.ErrNoRunners) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return false
	}
	u, _ := url.Parse(strings.TrimRight(run.HTTPBaseURL, "/"))
	proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, nil)
	if limitBody {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
	}
	proxy.ServeHTTP(w, r)
	return true
}

func sandboxProxyHandler(s store.SandboxStore, cfg *config.APIConfig) func(bool) http.HandlerFunc {
	return func(limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !isValidUUID(id) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}

			rec, err := s.Get(id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if rec == nil || !canAccessSandbox(r, rec) {
				writeError(w, http.StatusNotFound, "sandbox not found")
				return
			}

			if rec.RunnerHTTPBase == "" {
				writeError(w, http.StatusBadGateway, "sandbox has no runner routing information")
				return
			}

			if isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
				writeError(w, http.StatusNotFound, "sandbox not found")
				return
			}

			fields := obs.FieldsFrom(r.Context())
			fields.Add("sandbox_id", id, "runner_id", rec.RunnerID)

			u, _ := url.Parse(strings.TrimRight(rec.RunnerHTTPBase, "/"))
			upstreamStart := time.Now()
			proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, func(resp *http.Response) {
				// Response headers are in, so for a streamed exec this is the
				// time to first byte rather than the full round trip.
				fields.Add("ttfb_ms", time.Since(upstreamStart).Milliseconds())
				if reapSandboxIfRunnerGone(s, id, resp) {
					return
				}
				markSandboxActive(s, id, resp.StatusCode)
			})
			if limitBody {
				r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			}
			proxy.ServeHTTP(w, r)
		}
	}
}

// State-managed handlers that coordinate with runner service

type SandboxResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
}

type createSandboxRequest struct {
	ID *string `json:"id"`
}

func sandboxResponse(rec *store.SandboxRecord) *SandboxResponse {
	return &SandboxResponse{
		ID:           rec.ID,
		Status:       rec.Status,
		CreatedAt:    rec.CreatedAt,
		LastActiveAt: rec.LastActiveAt,
	}
}

func handleListSandboxes(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := authFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		var (
			records []*store.SandboxRecord
			err     error
		)
		if id.Role == roleAdmin {
			records, err = s.List()
		} else {
			records, err = s.ListByTenant(id.TenantID)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]*SandboxResponse, len(records))
		for i, rec := range records {
			resp[i] = &SandboxResponse{
				ID:           rec.ID,
				Status:       rec.Status,
				CreatedAt:    rec.CreatedAt,
				LastActiveAt: rec.LastActiveAt,
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleGetSandbox(s store.SandboxStore, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil || !canAccessSandbox(r, rec) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		resp := &SandboxResponse{
			ID:           rec.ID,
			Status:       rec.Status,
			CreatedAt:    rec.CreatedAt,
			LastActiveAt: rec.LastActiveAt,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func isPastIdleDeleteWindow(rec *store.SandboxRecord, cfg *config.APIConfig, now int64) bool {
	if rec == nil || cfg.IdleDeleteAfter <= 0 {
		return false
	}
	return now > rec.LastActiveAt+int64(cfg.IdleDeleteAfter.Seconds())
}

func runnerControlTLS(cfg *config.APIConfig) *runnerctl.TLS {
	if cfg.RunnerControlGRPCClientCAFile == "" {
		return nil
	}
	return &runnerctl.TLS{
		CAFile:     cfg.RunnerControlGRPCClientCAFile,
		CertFile:   cfg.RunnerControlGRPCClientCertFile,
		KeyFile:    cfg.RunnerControlGRPCClientKeyFile,
		ServerName: cfg.RunnerControlGRPCClientServerName,
	}
}

func handleCreateSandbox(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, rec *metrics.APIRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { rec.ObserveSandboxOp(metrics.OpCreate, success) }()

		authID, ok := authFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}

		var req createSandboxRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		sandboxID := generateUUID()
		if req.ID != nil {
			if !isValidUUID(*req.ID) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}
			sandboxID = *req.ID

			unlock, err := s.LockSandbox(r.Context(), sandboxID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			defer unlock()

			existing, err := s.Get(sandboxID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if existing != nil {
				if !canAccessSandbox(r, existing) {
					writeError(w, http.StatusConflict, "sandbox id unavailable")
					return
				}
				if !isPastIdleDeleteWindow(existing, cfg, time.Now().Unix()) {
					writeJSON(w, http.StatusOK, sandboxResponse(existing))
					success = true
					return
				}
				if !deleteSandboxRecord(w, r, s, cfg, existing) {
					return
				}
			}
		}

		tenantID := store.AdminTenantID
		if authID.Role == roleTenant {
			tenantID = authID.TenantID
			tenant, err := s.GetTenant(tenantID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tenant == nil {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			if tenant.MaxSandboxes > 0 {
				n, err := s.CountByTenant(tenantID)
				if err != nil {
					writeError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if n >= int64(tenant.MaxSandboxes) {
					writeError(w, http.StatusForbidden, "tenant sandbox quota exceeded")
					return
				}
			}
		}

		run, err := reg.PickLowestUsed()
		if err != nil {
			if errors.Is(err, registry.ErrNoRunners) {
				slog.WarnContext(r.Context(), "create sandbox failed: no eligible runners")
				writeError(w, http.StatusServiceUnavailable, err.Error())
			} else {
				slog.ErrorContext(r.Context(), "create sandbox failed: pick runner", "error", err)
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		controlAddr := run.ControlGRPCAddr
		tlsCfg := runnerControlTLS(cfg)

		fields := obs.FieldsFrom(r.Context())
		fields.Add("sandbox_id", sandboxID, "runner_id", run.ID)

		now := time.Now().Unix()
		slog.InfoContext(
			r.Context(),
			"create sandbox: runner selected",
			"sandbox_id", sandboxID,
			"runner_id", run.ID,
			"runner_http_base_url", run.HTTPBaseURL,
			"runner_control_grpc_addr", controlAddr,
			"runner_healthy", run.Healthy,
			"runner_capacity_total", run.CapacityTotal,
			"runner_capacity_used", run.CapacityUsed,
			"runner_capacity_stopped", run.CapacityStopped,
			"tenant_id", tenantID,
		)
		gresp, err := runnerctl.CreateSandbox(r.Context(), controlAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID, "{}")
		if err != nil {
			slog.ErrorContext(
				r.Context(),
				"create sandbox failed: runner control create",
				"sandbox_id", sandboxID,
				"runner_id", run.ID,
				"runner_control_grpc_addr", controlAddr,
				"error", err,
			)
			writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
			return
		}
		containerIP := gresp.GetContainerIp()

		record := &store.SandboxRecord{
			ID:                    sandboxID,
			Status:                "running",
			CreatedAt:             now,
			LastActiveAt:          now,
			ContainerIP:           containerIP,
			DaemonPort:            8081,
			RunnerID:              run.ID,
			RunnerHTTPBase:        strings.TrimRight(run.HTTPBaseURL, "/"),
			RunnerControlGRPCAddr: controlAddr,
			TenantID:              tenantID,
		}
		if err := s.Create(record); err != nil {
			_ = runnerctl.DeleteSandbox(r.Context(), controlAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID)
			if existing, getErr := s.Get(sandboxID); getErr == nil && existing != nil {
				if canAccessSandbox(r, existing) {
					writeJSON(w, http.StatusOK, sandboxResponse(existing))
					success = true
					return
				}
				writeError(w, http.StatusConflict, "sandbox id unavailable")
				return
			}
			slog.ErrorContext(
				r.Context(),
				"create sandbox failed: store record",
				"sandbox_id", sandboxID,
				"runner_id", run.ID,
				"container_ip", containerIP,
				"error", err,
			)
			if errors.Is(err, store.ErrTenantNotFound) {
				writeError(w, http.StatusConflict, "tenant not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to store sandbox: "+err.Error())
			return
		}
		slog.InfoContext(
			r.Context(),
			"create sandbox succeeded",
			"sandbox_id", sandboxID,
			"runner_id", run.ID,
			"container_ip", containerIP,
		)

		writeJSON(w, http.StatusCreated, sandboxResponse(record))
		success = true
	}
}

func deleteSandboxRecord(w http.ResponseWriter, r *http.Request, s store.SandboxStore, cfg *config.APIConfig, rec *store.SandboxRecord) bool {
	if err := runnerctl.DeleteSandbox(r.Context(), rec.RunnerControlGRPCAddr, cfg.RunnerAPIKey, runnerControlTLS(cfg), rec.ID); err != nil {
		writeError(w, http.StatusBadGateway, "failed to delete container: "+err.Error())
		return false
	}
	if err := s.Delete(rec.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}

func handleDeleteSandbox(s store.SandboxStore, cfg *config.APIConfig, mrec *metrics.APIRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { mrec.ObserveSandboxOp(metrics.OpDelete, success) }()

		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		unlock, err := s.LockSandbox(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer unlock()

		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil || !canAccessSandbox(r, rec) {
			// Same 404 as GET for missing and inaccessible — do not leak
			// cross-tenant existence via 204 vs 404.
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}

		if !deleteSandboxRecord(w, r, s, cfg, rec) {
			return
		}

		w.WriteHeader(http.StatusNoContent)
		success = true
	}
}

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func generateUUID() string {
	return uuid.New().String()
}

func isValidUUID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}

func newRunnerReverseProxy(runnerURL *url.URL, runnerAPIKey string, onResponse func(*http.Response)) *httputil.ReverseProxy {
	target := *runnerURL
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&target)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
			if runnerAPIKey != "" {
				pr.Out.Header.Set("X-Api-Key", runnerAPIKey)
			} else {
				pr.Out.Header.Del("X-Api-Key")
			}
			// Forward the trace context we established, never the caller's raw
			// header, which LoggingMiddleware has already validated or replaced.
			if tp := obs.Traceparent(pr.In.Context()); tp != "" {
				pr.Out.Header.Set(obs.HeaderTraceparent, tp)
			} else {
				pr.Out.Header.Del(obs.HeaderTraceparent)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if onResponse != nil {
				onResponse(resp)
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
				return
			}
			if strings.Contains(err.Error(), "request body too large") {
				writeError(w, http.StatusBadRequest, "failed to read request body: http: request body too large")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		},
	}
}

// reapSandboxIfRunnerGone deletes the store record when the runner's response
// says it no longer knows the sandbox.
//
// It returns the runner's verdict, not the outcome of the delete: true means
// "the runner says this sandbox is gone", so a failed delete still returns
// true. Callers use that to skip markSandboxActive, because a gone response is
// never evidence of a working sandbox — counting it as activity would let a
// retrying client refresh last_active_at and keep the idle sweeper from ever
// reclaiming the row left behind.
func reapSandboxIfRunnerGone(s store.SandboxStore, sandboxID string, resp *http.Response) bool {
	if !sandboxproxy.RunnerReportsSandboxGone(resp) {
		return false
	}
	if err := s.Delete(sandboxID); err != nil {
		slog.Error("remove sandbox after runner not-found", "sandbox_id", sandboxID, "err", err)
		return true
	}
	slog.Info("removed sandbox record after runner not-found", "sandbox_id", sandboxID)
	return true
}

// markSandboxActive records that a proxied request reached a working sandbox.
// Server errors deliberately do not count. The idle sweeper only stops a
// sandbox once last_active_at has gone stale, and only deletes one it has
// already stopped, so a client retrying a broken sandbox must not be able to
// refresh last_active_at or flip the status back to running — either would
// keep the sweeper from ever reclaiming it.
func markSandboxActive(s store.SandboxStore, sandboxID string, statusCode int) {
	if statusCode >= http.StatusInternalServerError {
		return
	}
	_ = s.UpdateLastActive(sandboxID)
	_ = s.UpdateStatus(sandboxID, "running")
}
