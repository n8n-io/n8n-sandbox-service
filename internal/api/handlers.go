package api

import (
	"context"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// newRunnerTransport builds the transport the API uses to reach runner HTTP
// listeners, presenting the same client certificate it uses for control gRPC.
//
// Build it once per process: it owns the connection pool, so a transport per
// request would mean a fresh TLS handshake per request.
//
// Returns a nil RoundTripper when the control-plane material is unset, matching
// runnerControlTLS. LoadAPI requires all three files, so that only happens in
// tests, which then fall back to the default transport. The return type is the
// interface rather than *http.Transport so that nil case stays a nil interface,
// which is what ReverseProxy checks before substituting its default.
func newRunnerTransport(cfg *config.APIConfig) (http.RoundTripper, error) {
	if cfg.RunnerControlGRPCClientCAFile == "" {
		return nil, nil
	}
	// Deliberately no ServerName: one transport serves every runner, and with
	// ServerName empty net/http verifies each runner against the host in its own
	// advertised base URL (docs/security-model.md, "API to runner").
	tlsConf, err := grpctls.NewClientTLSConfig(
		cfg.RunnerControlGRPCClientCAFile,
		cfg.RunnerControlGRPCClientCertFile,
		cfg.RunnerControlGRPCClientKeyFile,
		"",
	)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	return tr, nil
}

func sandboxProxyHandler(s store.SandboxStore, cfg *config.APIConfig, transport http.RoundTripper, arch *archiver) func(bool) http.HandlerFunc {
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

			// Fence before routing checks: a sandbox past its delete window is
			// 404 regardless of whether its row still has routing information.
			if isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
				writeError(w, http.StatusNotFound, "sandbox not found")
				return
			}
			// An archived sandbox comes back on any runner; the request that
			// finds it starts the restore and is told to come back.
			if rec.Status == statusArchived || rec.Status == statusRestoring {
				if arch != nil {
					arch.startRestore(rec)
				}
				writeRestoring(w)
				return
			}

			if rec.RunnerHTTPBase == "" {
				writeError(w, http.StatusBadGateway, "sandbox has no runner routing information")
				return
			}

			fields := obs.FieldsFrom(r.Context())
			fields.Add("sandbox_id", id, "runner_id", rec.RunnerID)

			// Re-check the scheme even though registration rejects non-https:
			// this record may predate that rule, and proxying to an http base
			// would send the runner API key in the clear, because
			// Transport.TLSClientConfig applies only to https URLs.
			u, err := url.Parse(strings.TrimRight(rec.RunnerHTTPBase, "/"))
			if err != nil || !strings.EqualFold(u.Scheme, "https") {
				writeError(w, http.StatusBadGateway, "sandbox runner is not reachable over https")
				return
			}

			upstreamStart := time.Now()
			proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, transport, func(resp *http.Response) {
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
	Ephemeral    bool   `json:"ephemeral"`
	Egress       string `json:"egress"`
	Template     string `json:"template"`
	// Seconds; left out when the sandbox uses the API's global window.
	IdleStopAfter    int64 `json:"idle_stop_after,omitempty"`
	IdleDeleteAfter  int64 `json:"idle_delete_after,omitempty"`
	IdleArchiveAfter int64 `json:"idle_archive_after,omitempty"`
	// ArchivedAt is when the sandbox was archived; present while archived or restoring.
	ArchivedAt int64 `json:"archived_at,omitempty"`
	// TenantID is the owning tenant, or store.AdminTenantID for a sandbox
	// created with an admin key. A tenant only ever sees its own id here.
	TenantID string `json:"tenant_id"`
}

func sandboxResponse(rec *store.SandboxRecord) *SandboxResponse {
	tenantID := rec.TenantID
	if store.IsAdminTenantID(tenantID) {
		// Rows from before tenants exist are backfilled on startup, but keep the
		// wire value canonical regardless of what the store holds.
		tenantID = store.AdminTenantID
	}
	return &SandboxResponse{
		ID:           rec.ID,
		Status:       rec.Status,
		CreatedAt:    rec.CreatedAt,
		LastActiveAt: rec.LastActiveAt,
		Ephemeral:    rec.Ephemeral,
		Egress:       rec.Egress,
		Template:     recordTemplate(rec),
		// Not acted on yet: the sweeper uses the global windows.
		IdleStopAfter:    rec.IdleStopAfter,
		IdleDeleteAfter:  rec.IdleDeleteAfter,
		IdleArchiveAfter: rec.IdleArchiveAfter,
		ArchivedAt:       rec.ArchivedAt,
		TenantID:         tenantID,
	}
}

// listTenantFilter reads the admin-only `tenant_id` query parameter of
// GET /sandboxes. ok is false when a response has already been written.
func listTenantFilter(w http.ResponseWriter, r *http.Request, role authRole) (tenantID string, ok bool) {
	values := r.URL.Query()["tenant_id"]
	if len(values) == 0 {
		return "", true
	}
	// A tenant key's listing is already scoped to itself; accepting the
	// parameter from it would only offer a way to probe other tenants' ids.
	if role != roleAdmin {
		writeError(w, http.StatusForbidden, "filtering by tenant_id requires an admin API key")
		return "", false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "tenant_id may be given once")
		return "", false
	}
	tenantID = values[0]
	if tenantID == store.AdminTenantID {
		return tenantID, true
	}
	if !isValidUUID(tenantID) {
		writeError(w, http.StatusBadRequest, "invalid tenant_id: expected a tenant UUID or "+store.AdminTenantID)
		return "", false
	}
	return tenantID, true
}

func handleListSandboxes(s store.SandboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := authFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		// Explicit per role: a fallthrough here would hand a new role the
		// tenant listing (or, in create, the admin pseudo-tenant).
		if id.Role != roleAdmin && id.Role != roleTenant {
			writeError(w, http.StatusForbidden, "sandbox access requires an admin or tenant API key")
			return
		}
		filter, ok := listTenantFilter(w, r, id.Role)
		if !ok {
			return
		}
		var (
			records []*store.SandboxRecord
			err     error
		)
		switch {
		case id.Role == roleTenant:
			records, err = s.ListByTenant(id.TenantID)
		case filter != "":
			records, err = s.ListByTenant(filter)
		default:
			records, err = s.List()
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]*SandboxResponse, len(records))
		for i, rec := range records {
			resp[i] = sandboxResponse(rec)
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
		writeJSON(w, http.StatusOK, sandboxResponse(rec))
	}
}

func isPastIdleDeleteWindow(rec *store.SandboxRecord, cfg *config.APIConfig, now int64) bool {
	if rec == nil {
		return false
	}
	phase := store.IdleDelete
	if rec.Ephemeral {
		phase = store.IdleEphemeral
	}
	window := idleWindow(rec, phase, cfg)
	return window > 0 && now > rec.LastActiveAt+idleSeconds(window)
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

// runnerCreateBudget is the deadline for the runner create RPC, used instead of
// the client's request context: a client disconnect must not cancel the RPC,
// because the runner would finish the create anyway and the resulting sandbox,
// with no store row, would hold a slot that quota and the idle sweeper cannot
// see. The runtime's own create budget plus a margin for the round trip.
const runnerCreateBudget = runnerruntime.CreateBudget + time.Minute

func handleCreateSandbox(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, rec *metrics.APIRecorder, quota *tenantQuota, arch *archiver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { rec.ObserveSandboxOp(metrics.OpCreate, success) }()

		authID, ok := authFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		// Before any work: the tenant resolution below defaults to the admin
		// pseudo-tenant, which must never apply to a role other than admin.
		if authID.Role != roleAdmin && authID.Role != roleTenant {
			writeError(w, http.StatusForbidden, "sandbox access requires an admin or tenant API key")
			return
		}

		var req createSandboxRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCreateBodyBytes))
		if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var limitErr error
		params, err := parseCreateRequest(&req, cfg, func() (int64, error) {
			limit, err := idleWindowLimit(s, cfg, authID.TenantID)
			limitErr = err
			return limit, err
		})
		if limitErr != nil {
			writeError(w, http.StatusInternalServerError, limitErr.Error())
			return
		}
		if err != nil {
			var ce *createError
			if errors.As(err, &ce) {
				writeErrorReason(w, http.StatusBadRequest, ce.msg, ce.reason)
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		egress := params.egress

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
					success = writeExistingSandbox(w, existing, params)
					return
				}
				if !deleteSandboxRecord(w, r, s, cfg, existing, arch) {
					return
				}
			}
		}

		tenantID := store.AdminTenantID
		releaseQuota := func(bool) {}
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
			var ok bool
			releaseQuota, ok, err = quota.reserve(tenantID, tenant.MaxSandboxes)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !ok {
				writeError(w, http.StatusForbidden, "tenant sandbox quota exceeded")
				return
			}
		}
		// A create that is not saved frees its slot on return, after any
		// runner rollback; a saved one right after the save, below.
		defer releaseQuota(false)

		requested := params.runnerOptions(tenantID)
		createOptionsJSON, err := json.Marshal(requested)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		obs.FieldsFrom(r.Context()).Add("sandbox_id", sandboxID)
		now := time.Now().Unix()
		placed, err := placeSandbox(r, reg, cfg, rec, sandboxID, params.template, string(createOptionsJSON),
			"tenant_id", tenantID, "ephemeral", req.Ephemeral, "egress", egress, "template", params.template)
		if errors.Is(err, registry.ErrNoRunners) || errors.Is(err, errRunnersAtCapacity) {
			writeNoCapacity(w, err)
			return
		}
		// The runner refused before creating anything: it cannot serve a
		// valid option yet (egress proxy). A fixed message: the runner's own
		// (or an intermediary's) is logged by placeSandbox, not relayed.
		// The runner refused a secret the tenant does not have, before creating
		// anything.
		if status.Code(err) == codes.FailedPrecondition {
			writeErrorReason(w, http.StatusBadRequest, "egress_proxy names a secret_ref the tenant does not have", reasonUnknownSecretRef)
			return
		}
		if status.Code(err) == codes.Unimplemented {
			writeErrorReason(w, http.StatusNotImplemented, "the requested egress is not supported by the sandbox runners", reasonNotImplemented)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		run, gresp := placed.run, placed.resp
		controlAddr := run.ControlGRPCAddr
		tlsCfg := runnerControlTLS(cfg)
		// Undoes the runner create when the sandbox cannot be tracked as asked.
		// Fresh deadline: the create may have used up most of its own.
		rollbackRunnerCreate := func() {
			deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(r.Context()), runnerCreateBudget)
			delErr := runnerctl.DeleteSandbox(deleteCtx, controlAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID)
			cancelDelete()
			if delErr != nil {
				slog.ErrorContext(
					r.Context(),
					"create sandbox failed: compensating runner delete, sandbox is untracked",
					"sandbox_id", sandboxID,
					"runner_id", run.ID,
					"runner_control_grpc_addr", controlAddr,
					"error", delErr,
				)
			}
		}

		// The runner echoes the options it applied; a runner from before egress
		// modes echoes nothing and applied the defaults. Anything but what was
		// asked for would leave the record saying one thing and the network
		// another, so the sandbox is removed rather than tracked.
		applied, err := runnerruntime.ParseCreateOptions(gresp.GetAppliedCreateOptionsJson())
		if err != nil || !applied.Equal(&requested) {
			rollbackRunnerCreate()
			// Not the echoed JSON itself: an egress_proxy spec names secrets.
			appliedEgress, appliedTemplate, appliedProxy := "", "", false
			if applied != nil {
				appliedEgress, appliedTemplate, appliedProxy = string(applied.Egress), applied.Template, applied.EgressProxy != nil
			}
			slog.ErrorContext(
				r.Context(),
				"create sandbox failed: runner did not apply the requested egress",
				"sandbox_id", sandboxID,
				"runner_id", run.ID,
				"requested_egress", egress,
				"applied_egress", appliedEgress,
				"applied_template", appliedTemplate,
				"applied_egress_proxy", appliedProxy,
				"error", err,
			)
			writeError(w, http.StatusBadGateway, "runner did not apply the requested egress")
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
			Ephemeral:             req.Ephemeral,
			Egress:                string(egress),
			Template:              params.template,
			IdleStopAfter:         params.idleStopAfter,
			IdleDeleteAfter:       params.idleDeleteAfter,
			IdleArchiveAfter:      params.idleArchiveAfter,
			EgressProxy:           params.egressProxyJSON,
		}
		if err := s.Create(record); err != nil {
			rollbackRunnerCreate()
			// Unless the store refused the row, it may have been saved anyway,
			// say when a commit's reply was lost: count it as saved, the safe side.
			releaseQuota(!errors.Is(err, store.ErrTenantQuotaExceeded) && !errors.Is(err, store.ErrTenantNotFound))
			if existing, getErr := s.Get(sandboxID); getErr == nil && existing != nil {
				if !canAccessSandbox(r, existing) {
					writeError(w, http.StatusConflict, "sandbox id unavailable")
					return
				}
				success = writeExistingSandbox(w, existing, params)
				return
			}
			if errors.Is(err, store.ErrTenantQuotaExceeded) {
				// Passed the check above, but creates on other API pods
				// were saved first and filled the quota, or it was lowered.
				slog.WarnContext(
					r.Context(),
					"create sandbox refused: tenant quota filled by concurrent creates",
					"sandbox_id", sandboxID,
					"runner_id", run.ID,
					"tenant_id", tenantID,
				)
				writeError(w, http.StatusForbidden, "tenant sandbox quota exceeded")
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
		releaseQuota(true)
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

// writeExistingSandbox answers a create whose id the caller already owns with
// that sandbox, or 409 when the request names options the sandbox was not
// created with (createParams.conflict).
func writeExistingSandbox(w http.ResponseWriter, existing *store.SandboxRecord, params *createParams) bool {
	if msg, reason := params.conflict(existing); msg != "" {
		writeErrorReason(w, http.StatusConflict, msg, reason)
		return false
	}
	writeJSON(w, http.StatusOK, sandboxResponse(existing))
	return true
}

func deleteSandboxRecord(w http.ResponseWriter, r *http.Request, s store.SandboxStore, cfg *config.APIConfig, rec *store.SandboxRecord, arch *archiver) bool {
	switch rec.Status {
	case statusRestoring:
		// The restore would recreate it on a runner, untracked.
		writeRestoring(w)
		return false
	case statusArchived:
		if arch == nil {
			writeError(w, http.StatusInternalServerError, "sandbox is archived but no archive store is configured")
			return false
		}
		if err := arch.deleteArchived(r.Context(), rec); err != nil {
			slog.WarnContext(r.Context(), "delete archived sandbox", "sandbox_id", rec.ID, "err", err)
			writeError(w, http.StatusBadGateway, "failed to delete archived sandbox")
			return false
		}
		return true
	}
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

func handleDeleteSandbox(s store.SandboxStore, cfg *config.APIConfig, mrec *metrics.APIRecorder, arch *archiver) http.HandlerFunc {
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

		if !deleteSandboxRecord(w, r, s, cfg, rec, arch) {
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

// errRunnerRedirect marks a 3xx from the runner, which the proxy refuses: no
// runner route redirects, and relaying one would send a client, and the API key
// it holds, wherever a compromised runner points.
var errRunnerRedirect = errors.New("runner returned a redirect")

func newRunnerReverseProxy(runnerURL *url.URL, runnerAPIKey string, transport http.RoundTripper, onResponse func(*http.Response)) *httputil.ReverseProxy {
	target := *runnerURL
	return &httputil.ReverseProxy{
		Transport: transport,
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
			// Before onResponse, so a redirect never counts as sandbox activity.
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				loc := resp.Header.Get("Location")
				if len(loc) > 200 {
					loc = loc[:200]
				}
				obs.FieldsFrom(resp.Request.Context()).Add("runner_redirect_status", resp.StatusCode, "runner_redirect_location", loc)
				return errRunnerRedirect
			}
			if onResponse != nil {
				onResponse(resp)
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errRunnerRedirect) {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
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
