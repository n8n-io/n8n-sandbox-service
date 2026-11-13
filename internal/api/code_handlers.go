package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// codeTaskMaxBodyBytes bounds a task request at the API: 1 GiB of input
// binaries plus the other inputs.
const codeTaskMaxBodyBytes int64 = 2200 * 1024 * 1024

// codeRPCResultMaxBodyBytes bounds the result of one RPC call.
const codeRPCResultMaxBodyBytes int64 = 1 << 30

// Defaults of POST /v1/code/sandboxes.
const (
	defaultCodeRuntime      = "n8n-js"
	defaultCodeSlots        = 1
	defaultCodeTaskMemoryMB = 512
)

// codeDrainGraceSeconds is how long a draining sandbox must have no request
// before the sweeper deletes it. It lets the client read the outputs of a task
// that just ended.
const codeDrainGraceSeconds = 2

// minCodeLifetimeMs is the smallest idleTimeoutMs and maxAgeMs.
const minCodeLifetimeMs = 1000

var codeNameRegex = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type createCodeSandboxRequest struct {
	Name          string            `json:"name"`
	Runtime       string            `json:"runtime"`
	Slots         int               `json:"slots"`
	TaskMemoryMB  int               `json:"taskMemoryMb"`
	SpareWorkers  *bool             `json:"spareWorkers"`
	ReuseWorkers  bool              `json:"reuseWorkers"`
	IdleTimeoutMs int64             `json:"idleTimeoutMs"`
	MaxAgeMs      int64             `json:"maxAgeMs"`
	Labels        map[string]string `json:"labels"`
}

type codeCreateTimings struct {
	// Create is the full create time at the API, in milliseconds.
	Create int64 `json:"create"`
	// RunnerCreate is the runner create RPC: container create and start,
	// network rules, and the wait for the code daemon.
	RunnerCreate int64 `json:"runnerCreate"`
	// DaemonReady and SpareReady are measured by the code daemon from its own
	// process start.
	DaemonReady int64 `json:"daemonReady"`
	SpareReady  int64 `json:"spareReady"`
	// Claim is the time to take a sandbox from the warm pool, in milliseconds.
	// Only set when the sandbox came from the pool.
	Claim int64 `json:"claim,omitempty"`
}

type codeSandboxResponse struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Created is false when the name already had a current sandbox (create only).
	Created        *bool  `json:"created,omitempty"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
	Slots          int    `json:"slots"`
	State          string `json:"state"`
	Pooled         bool   `json:"pooled,omitempty"`
	BusySlots      *int   `json:"busySlots,omitempty"`
	WorkersStarted *int64 `json:"workersStarted,omitempty"`
	// Resource use of the sandbox container since its start (GET only).
	CPUUsageUsec       *int64             `json:"cpuUsageUsec,omitempty"`
	MemoryPeakBytes    *int64             `json:"memoryPeakBytes,omitempty"`
	MemoryCurrentBytes *int64             `json:"memoryCurrentBytes,omitempty"`
	UptimeMs           *int64             `json:"uptimeMs,omitempty"`
	Timings            *codeCreateTimings `json:"timings,omitempty"`
}

// codeDaemonState is the subset of the code daemon /state and /healthz answer
// that the API reads.
type codeDaemonState struct {
	Status             string `json:"status"`
	Runtime            string `json:"runtime"`
	RuntimeVersion     string `json:"runtimeVersion"`
	Slots              int    `json:"slots"`
	BusySlots          int    `json:"busySlots"`
	WorkersStarted     int64  `json:"workersStarted"`
	CPUUsageUsec       int64  `json:"cpuUsageUsec"`
	MemoryPeakBytes    int64  `json:"memoryPeakBytes"`
	MemoryCurrentBytes int64  `json:"memoryCurrentBytes"`
	UptimeMs           int64  `json:"uptimeMs"`
	Timings            struct {
		DaemonReady int64 `json:"daemonReady"`
		SpareReady  int64 `json:"spareReady"`
	} `json:"timings"`
}

// codeActivity counts running Task API requests per code sandbox. The code
// idle sweeper does not delete a sandbox that has one. It is in memory: the
// spike runs one API pod.
type codeActivity struct {
	mu       sync.Mutex
	requests map[string]int
}

// codeActivityTracker is shared by the router and the code idle sweeper of the
// process.
var codeActivityTracker = newCodeActivity()

func newCodeActivity() *codeActivity {
	return &codeActivity{requests: make(map[string]int)}
}

func (a *codeActivity) begin(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests[id]++
}

func (a *codeActivity) end(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.requests[id] <= 1 {
		delete(a.requests, id)
	} else {
		a.requests[id]--
	}
}

func (a *codeActivity) busy(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests[id] > 0
}

// codeAPI holds the Task API handlers.
type codeAPI struct {
	s         store.SandboxStore
	reg       registry.RunnerRegistry
	cfg       *config.APIConfig
	rec       *metrics.APIRecorder
	transport http.RoundTripper
	client    *http.Client
	activity  *codeActivity
}

func newCodeAPI(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, rec *metrics.APIRecorder, transport http.RoundTripper, activity *codeActivity) *codeAPI {
	return &codeAPI{
		s: s, reg: reg, cfg: cfg, rec: rec, transport: transport, activity: activity,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

func (c *codeAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/code/sandboxes", c.handleCreate)
	mux.HandleFunc("GET /v1/code/sandboxes/{id}", c.handleGet)
	mux.HandleFunc("DELETE /v1/code/sandboxes/{id}", c.handleDelete)
	mux.HandleFunc("DELETE /v1/code/sandboxes/names/{name}", c.handleDeleteByName)
	mux.HandleFunc("POST /v1/code/sandboxes/{id}/tasks", c.proxy(codeTaskMaxBodyBytes, true))
	mux.HandleFunc("GET /v1/code/sandboxes/{id}/tasks/{taskId}/outputs/{path...}", c.proxy(0, false))
	mux.HandleFunc("DELETE /v1/code/sandboxes/{id}/tasks/{taskId}", c.proxy(0, false))
	mux.HandleFunc("POST /v1/code/sandboxes/{id}/tasks/{taskId}/rpc/{callId}", c.proxy(codeRPCResultMaxBodyBytes, false))
}

// handleCreate creates a code sandbox. It answers when the code daemon is ready.
func (c *codeAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	success := false
	defer func() { c.rec.ObserveSandboxOp(metrics.OpCreate, success) }()

	authID, ok := authFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	if authID.Role != roleAdmin && authID.Role != roleTenant {
		writeError(w, http.StatusForbidden, "sandbox access requires an admin or tenant API key")
		return
	}

	var req createCodeSandboxRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	requested, err := codeCreateOptions(req.Runtime, req.Slots, req.TaskMemoryMB, req.SpareWorkers, req.ReuseWorkers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	settings, err := c.codeSettings(&req, requested, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tenantID := store.AdminTenantID
	var tenant *store.Tenant
	if authID.Role == roleTenant {
		tenantID = authID.TenantID
		tenant, err = c.s.GetTenant(tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tenant == nil {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
	}

	// One create per name at a time. Concurrent creates of the name wait here,
	// then get the sandbox of the first one.
	if req.Name != "" {
		unlock, err := c.s.LockName(r.Context(), tenantID+"/"+req.Name)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "lock sandbox name: "+err.Error())
			return
		}
		defer unlock()
		cur, err := c.s.GetCurrentByName(tenantID, req.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cur != nil && codeMaxAgeReached(cur, time.Now()) {
			// Max age passed before the sweeper saw it: the name moves now.
			if err := c.s.MarkDraining(cur.ID, time.Now().Unix()); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			slog.InfoContext(r.Context(), "code sandbox draining", "sandbox_id", cur.ID, "reason", "max_age")
			cur = nil
		}
		if cur != nil {
			if cur.OptionsKey != settings.OptionsKey {
				writeError(w, http.StatusConflict, "sandbox name exists with other options")
				return
			}
			created := false
			obs.FieldsFrom(r.Context()).Add("sandbox_id", cur.ID, "runner_id", cur.RunnerID, "kind", store.KindCode)
			writeJSON(w, http.StatusOK, codeSandboxResponse{
				ID:      cur.ID,
				Name:    cur.Name,
				Created: &created,
				Runtime: requested.Runtime,
				Slots:   requested.Slots,
				State:   "ready",
				Timings: &codeCreateTimings{Create: time.Since(t0).Milliseconds()},
			})
			success = true
			return
		}
	}

	if tenant != nil {
		if tenant.MaxSandboxes > 0 {
			n, err := c.s.CountByTenant(tenantID)
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

	// A matching warm pool gives a ready sandbox without a create. The API
	// assumes that a pooled sandbox is healthy (WARM-POOL-DESIGN-opus.md).
	if pool := activeCodePool.Load(); pool != nil {
		tClaim := time.Now()
		rec, err := pool.claim(requested, tenantID)
		claimMs := time.Since(tClaim).Milliseconds()
		switch {
		case errors.Is(err, store.ErrTenantNotFound):
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "claim pooled code sandbox failed, creating instead", "error", err)
		case rec != nil:
			if err := c.s.SetCodeSettings(rec.ID, settings); err != nil {
				// The claimed sandbox has no name now. The idle sweeper deletes it.
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			created := true
			resp := codeSandboxResponse{
				ID:             rec.ID,
				Name:           settings.Name,
				Created:        &created,
				Runtime:        requested.Runtime,
				RuntimeVersion: pool.takeVersion(rec.ID),
				Slots:          requested.Slots,
				State:          "ready",
				Pooled:         true,
				Timings:        &codeCreateTimings{Claim: claimMs, Create: time.Since(t0).Milliseconds()},
			}
			obs.FieldsFrom(r.Context()).Add("sandbox_id", rec.ID, "runner_id", rec.RunnerID, "kind", store.KindCode)
			slog.InfoContext(r.Context(), "claim pooled code sandbox succeeded",
				"sandbox_id", rec.ID, "name", settings.Name, "runner_id", rec.RunnerID, "pool", codePoolKey(requested), "labels", req.Labels,
				"claim_ms", claimMs)
			writeJSON(w, http.StatusCreated, resp)
			success = true
			return
		}
	}

	run, err := c.reg.PickLowestUsed()
	if err != nil {
		if errors.Is(err, registry.ErrNoRunners) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	record, runnerCreateMs, err := createCodeSandboxOn(r.Context(), c.s, c.cfg, run, requested, tenantID, "", settings)
	if err != nil {
		var ce *codeCreateError
		if errors.As(err, &ce) {
			writeError(w, ce.status, ce.msg)
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	sandboxID := record.ID
	obs.FieldsFrom(r.Context()).Add("sandbox_id", sandboxID, "runner_id", run.ID, "kind", store.KindCode)

	created := true
	resp := codeSandboxResponse{
		ID:      sandboxID,
		Name:    settings.Name,
		Created: &created,
		Runtime: requested.Runtime,
		Slots:   requested.Slots,
		State:   "ready",
		Timings: &codeCreateTimings{RunnerCreate: runnerCreateMs},
	}
	if st, err := c.daemonState(r.Context(), record, "healthz"); err == nil {
		resp.RuntimeVersion = st.RuntimeVersion
		resp.Timings.DaemonReady = st.Timings.DaemonReady
		resp.Timings.SpareReady = st.Timings.SpareReady
	} else {
		slog.WarnContext(r.Context(), "create code sandbox: read daemon state", "sandbox_id", sandboxID, "error", err)
	}
	resp.Timings.Create = time.Since(t0).Milliseconds()
	slog.InfoContext(r.Context(), "create code sandbox succeeded",
		"sandbox_id", sandboxID, "name", settings.Name, "runner_id", run.ID, "runtime", requested.Runtime, "slots", requested.Slots,
		"task_memory_mb", requested.TaskMemoryMB, "spare_workers", requested.SpareWorkers, "reuse_workers", requested.ReuseWorkers, "labels", req.Labels,
		"create_ms", resp.Timings.Create, "runner_create_ms", runnerCreateMs)
	writeJSON(w, http.StatusCreated, resp)
	success = true
}

// codeSettings validates the name and the lifecycle fields of a create request.
func (c *codeAPI) codeSettings(req *createCodeSandboxRequest, requested runnerruntime.CreateOptions, now time.Time) (store.CodeSettings, error) {
	s := store.CodeSettings{Name: req.Name, OptionsKey: codePoolKey(requested)}
	if req.Name != "" && !codeNameRegex.MatchString(req.Name) {
		return s, errors.New("name must be 1 to 128 characters: letters, digits, '_', '.', ':' or '-'")
	}
	if req.IdleTimeoutMs < 0 || (req.IdleTimeoutMs > 0 && req.IdleTimeoutMs < minCodeLifetimeMs) {
		return s, fmt.Errorf("idleTimeoutMs must be 0 or at least %d", minCodeLifetimeMs)
	}
	if req.MaxAgeMs < 0 || (req.MaxAgeMs > 0 && req.MaxAgeMs < minCodeLifetimeMs) {
		return s, fmt.Errorf("maxAgeMs must be 0 or at least %d", minCodeLifetimeMs)
	}
	s.IdleTimeoutMs = req.IdleTimeoutMs
	// The configured idle window is the upper bound.
	if limit := c.cfg.CodeIdleDeleteAfter.Milliseconds(); limit > 0 && s.IdleTimeoutMs > limit {
		s.IdleTimeoutMs = limit
	}
	if req.MaxAgeMs > 0 {
		s.DrainAt = now.Unix() + idleSeconds(time.Duration(req.MaxAgeMs)*time.Millisecond)
	}
	if len(req.Labels) > 0 {
		b, err := json.Marshal(req.Labels)
		if err != nil {
			return s, err
		}
		s.Labels = string(b)
	}
	return s, nil
}

// codeMaxAgeReached reports whether the max age of rec has passed.
func codeMaxAgeReached(rec *store.SandboxRecord, now time.Time) bool {
	return rec.DrainAt > 0 && now.Unix() >= rec.DrainAt
}

// codeDraining reports whether rec takes no new tasks.
func codeDraining(rec *store.SandboxRecord, now time.Time) bool {
	return rec.DrainingSince > 0 || codeMaxAgeReached(rec, now)
}

// handleDeleteByName deletes all sandboxes of a name, also the draining ones.
// A later create of the name gets a new sandbox.
func (c *codeAPI) handleDeleteByName(w http.ResponseWriter, r *http.Request) {
	success := false
	defer func() { c.rec.ObserveSandboxOp(metrics.OpDelete, success) }()
	authID, ok := authFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	if authID.Role != roleAdmin && authID.Role != roleTenant {
		writeError(w, http.StatusForbidden, "sandbox access requires an admin or tenant API key")
		return
	}
	name := r.PathValue("name")
	if !codeNameRegex.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid sandbox name")
		return
	}
	tenantID := store.AdminTenantID
	if authID.Role == roleTenant {
		tenantID = authID.TenantID
	}
	records, err := c.s.ListByName(tenantID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tlsCfg := runnerControlTLS(c.cfg)
	var failed error
	for _, rec := range records {
		lockErr := withLockedSandbox(r.Context(), c.s, rec.ID, func(cur *store.SandboxRecord) {
			if cur.Kind != store.KindCode || cur.Name != name || !canAccessSandbox(r, cur) {
				return
			}
			if _, err := deleteIdleSandbox(r.Context(), c.s, c.reg, c.cfg, tlsCfg, cur, time.Now(), "code_delete_by_name"); err != nil {
				failed = err
			}
		})
		if lockErr != nil {
			failed = lockErr
		}
	}
	if failed != nil {
		writeError(w, http.StatusBadGateway, "failed to delete sandbox: "+failed.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
	success = true
}

// daemonState reads /healthz or /state of the code daemon through the runner.
func (c *codeAPI) daemonState(ctx context.Context, rec *store.SandboxRecord, route string) (*codeDaemonState, error) {
	return readCodeDaemonState(ctx, c.client, c.cfg, rec, route)
}

func readCodeDaemonState(ctx context.Context, client *http.Client, cfg *config.APIConfig, rec *store.SandboxRecord, route string) (*codeDaemonState, error) {
	u, err := runnerBaseURL(rec)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/code/sandboxes/"+rec.ID+"/"+route, nil)
	if err != nil {
		return nil, err
	}
	if cfg.RunnerAPIKey != "" {
		req.Header.Set("X-Api-Key", cfg.RunnerAPIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st codeDaemonState
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&st); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, errors.New("runner answered " + resp.Status)
	}
	return &st, nil
}

func runnerBaseURL(rec *store.SandboxRecord) (*url.URL, error) {
	if rec.RunnerHTTPBase == "" {
		return nil, errors.New("sandbox has no runner routing information")
	}
	u, err := url.Parse(strings.TrimRight(rec.RunnerHTTPBase, "/"))
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("sandbox runner is not reachable over https")
	}
	return u, nil
}

// lookupCode returns the code sandbox record, or writes 404.
func (c *codeAPI) lookupCode(w http.ResponseWriter, r *http.Request) *store.SandboxRecord {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid sandbox id")
		return nil
	}
	rec, err := c.s.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if rec == nil || !canAccessSandbox(r, rec) || rec.Kind != store.KindCode {
		writeError(w, http.StatusNotFound, "code sandbox not found")
		return nil
	}
	return rec
}

func (c *codeAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	rec := c.lookupCode(w, r)
	if rec == nil {
		return
	}
	st, err := c.daemonState(r.Context(), rec, "state")
	if err != nil {
		writeError(w, http.StatusBadGateway, "read code sandbox state: "+err.Error())
		return
	}
	busy := st.BusySlots
	started := st.WorkersStarted
	state := st.Status
	if codeDraining(rec, time.Now()) {
		state = "draining"
	}
	writeJSON(w, http.StatusOK, codeSandboxResponse{
		ID:                 rec.ID,
		Name:               rec.Name,
		Runtime:            st.Runtime,
		RuntimeVersion:     st.RuntimeVersion,
		Slots:              st.Slots,
		State:              state,
		BusySlots:          &busy,
		WorkersStarted:     &started,
		CPUUsageUsec:       &st.CPUUsageUsec,
		MemoryPeakBytes:    &st.MemoryPeakBytes,
		MemoryCurrentBytes: &st.MemoryCurrentBytes,
		UptimeMs:           &st.UptimeMs,
	})
}

func (c *codeAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	success := false
	defer func() { c.rec.ObserveSandboxOp(metrics.OpDelete, success) }()
	id := r.PathValue("id")
	if !isValidUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid sandbox id")
		return
	}
	unlock, err := c.s.LockSandbox(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer unlock()
	rec := c.lookupCode(w, r)
	if rec == nil {
		return
	}
	if !deleteSandboxRecord(w, r, c.s, c.cfg, rec) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
	success = true
}

var (
	codeTaskIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	codeCallIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// proxy forwards a Task API request to the runner, which forwards it to the
// code daemon. A route with a body limit may run for a long time: task
// requests and large RPC results. With newTask, a draining sandbox answers 410
// before the body is read. Running tasks of a draining sandbox keep their
// other routes.
func (c *codeAPI) proxy(maxBody int64, newTask bool) http.HandlerFunc {
	longLived := maxBody > 0
	return func(w http.ResponseWriter, r *http.Request) {
		rec := c.lookupCode(w, r)
		if rec == nil {
			return
		}
		if newTask && codeDraining(rec, time.Now()) {
			writeError(w, http.StatusGone, "code sandbox is draining")
			return
		}
		if taskID := r.PathValue("taskId"); taskID != "" && !codeTaskIDRegex.MatchString(taskID) {
			writeError(w, http.StatusBadRequest, "invalid task id")
			return
		}
		if callID := r.PathValue("callId"); callID != "" && !codeCallIDRegex.MatchString(callID) {
			writeError(w, http.StatusBadRequest, "invalid call id")
			return
		}
		out := r.Clone(r.Context())
		out.URL.Path = strings.TrimPrefix(r.URL.Path, "/v1")
		out.URL.RawPath = ""
		// The path is decoded here. An encoded "/.." must not reach the runner.
		for _, seg := range strings.Split(out.URL.Path, "/") {
			if seg == "." || seg == ".." {
				writeError(w, http.StatusBadRequest, "invalid path")
				return
			}
		}
		u, err := runnerBaseURL(rec)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		obs.FieldsFrom(r.Context()).Add("sandbox_id", rec.ID, "runner_id", rec.RunnerID)
		if longLived {
			// The server ReadTimeout would cut long uploads and task streams.
			_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		}

		c.activity.begin(rec.ID)
		healthy := false
		defer func() {
			c.activity.end(rec.ID)
			// Mark the end of a long request as activity, but never a failed
			// one: a client that retries a broken sandbox must not keep it.
			if healthy {
				_ = c.s.UpdateLastActive(rec.ID)
			}
		}()

		if maxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			out.Body = r.Body
		}
		proxy := newRunnerReverseProxy(u, c.cfg.RunnerAPIKey, c.transport, func(resp *http.Response) {
			if reapSandboxIfRunnerGone(c.s, rec.ID, resp) {
				return
			}
			healthy = resp.StatusCode < http.StatusInternalServerError
			markSandboxActive(c.s, rec.ID, resp.StatusCode)
		})
		proxy.ServeHTTP(w, out)
	}
}

// StartCodeIdleSweeper runs the lifecycle of code sandboxes:
//   - It deletes a sandbox that has had no Task API request for its idle
//     timeout (idleTimeoutMs, else cfg.CodeIdleDeleteAfter). This removes the
//     sandboxes that a crashed client left behind.
//   - It marks a sandbox as draining when its max age (maxAgeMs) has passed,
//     and deletes a draining sandbox when no request uses it.
func StartCodeIdleSweeper(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig) {
	activity := codeActivityTracker
	window := cfg.CodeIdleDeleteAfter
	interval := 5 * time.Second
	if window > 0 && window/4 < interval {
		interval = max(window/4, time.Second)
	}
	slog.Info("code sandbox sweeper enabled", "idle_delete_after", window.String(), "interval", interval.String())
	tlsCfg := runnerControlTLS(cfg)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				sweepIdleCodeSandboxes(ctx, s, reg, cfg, tlsCfg, activity, window, now)
			}
		}
	}()
}

func sweepIdleCodeSandboxes(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, tlsCfg *runnerctl.TLS, activity *codeActivity, window time.Duration, now time.Time) {
	records, err := s.List()
	if err != nil {
		slog.Error("code idle sweep: list sandboxes", "err", err)
		return
	}
	// The pool manager owns pooled sandboxes, which get no activity. Without a
	// manager (pools removed from the config), they are idle like any other.
	poolActive := activeCodePool.Load() != nil
	action := func(r *store.SandboxRecord) string {
		if r.Kind != store.KindCode || (poolActive && r.TenantID == store.PoolTenantID) {
			return ""
		}
		return codeSweepAction(r, activity, window, now)
	}
	for _, rec := range records {
		if ctx.Err() != nil {
			return
		}
		if action(rec) == "" {
			continue
		}
		_ = withLockedSandbox(ctx, s, rec.ID, func(cur *store.SandboxRecord) {
			switch a := action(cur); a {
			case "drain":
				if err := s.MarkDraining(cur.ID, now.Unix()); err != nil {
					slog.Error("code sweep: mark draining", "sandbox_id", cur.ID, "err", err)
					return
				}
				slog.Info("code sandbox draining", "sandbox_id", cur.ID, "name", cur.Name, "reason", "max_age")
			case "code_idle", "code_drained":
				_, _ = deleteIdleSandbox(ctx, s, reg, cfg, tlsCfg, cur, now, a)
			}
		})
	}
}

// codeSweepAction returns what the sweeper does with a code sandbox: "drain"
// (max age passed), "code_idle" (idle timeout passed), "code_drained" (draining
// and unused), or "" (nothing).
func codeSweepAction(r *store.SandboxRecord, activity *codeActivity, defaultIdle time.Duration, now time.Time) string {
	if r.DrainingSince == 0 && codeMaxAgeReached(r, now) {
		return "drain"
	}
	if activity.busy(r.ID) {
		return ""
	}
	idle := defaultIdle
	if r.IdleTimeoutMs > 0 {
		idle = time.Duration(r.IdleTimeoutMs) * time.Millisecond
	}
	if idle > 0 && r.LastActiveAt <= now.Unix()-idleSeconds(idle) {
		return "code_idle"
	}
	if r.DrainingSince > 0 && r.LastActiveAt <= now.Unix()-codeDrainGraceSeconds {
		return "code_drained"
	}
	return ""
}
