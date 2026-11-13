package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// reasonSettingsMismatch marks a 409 for a create whose name already points to
// a code sandbox created with other settings.
const reasonSettingsMismatch = "sandbox_settings_mismatch"

// Code sandbox runtimes.
const (
	codeRuntimeJS     = "n8n-js"
	codeRuntimePython = "n8n-python"
)

// maxCodeCreateBody bounds the create request body.
const maxCodeCreateBody = 4096

var (
	codeNameRegex           = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	codeRuntimeVersionRegex = regexp.MustCompile(`^[A-Za-z0-9.+-]{1,64}$`)
)

type createCodeSandboxRequest struct {
	Name           string `json:"name"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
	Slots          int    `json:"slots"`
	MemoryMb       int    `json:"memoryMb"`
	IdleTimeoutMs  int64  `json:"idleTimeoutMs"`
	MaxAgeMs       int64  `json:"maxAgeMs"`
}

func (req *createCodeSandboxRequest) validate() error {
	switch {
	case !codeNameRegex.MatchString(req.Name):
		return errors.New("invalid name: 1 to 128 characters from A-Z, a-z, 0-9, '.', '_', ':' and '-'")
	case req.Runtime != codeRuntimeJS && req.Runtime != codeRuntimePython:
		return fmt.Errorf("invalid runtime: must be %q or %q", codeRuntimeJS, codeRuntimePython)
	case !codeRuntimeVersionRegex.MatchString(req.RuntimeVersion):
		return errors.New("invalid runtimeVersion")
	case req.Slots <= 0:
		return errors.New("invalid slots: must be greater than 0")
	case req.MemoryMb <= 0:
		return errors.New("invalid memoryMb: must be greater than 0")
	case req.IdleTimeoutMs < 0:
		return errors.New("invalid idleTimeoutMs: must not be negative")
	case req.MaxAgeMs < 0:
		return errors.New("invalid maxAgeMs: must not be negative")
	}
	return nil
}

// codeSandboxSettings are the request fields a code sandbox is created with,
// all but the name. Creates of one name get the current sandbox only when
// these are equal.
type codeSandboxSettings struct {
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
	Slots          int    `json:"slots"`
	MemoryMb       int    `json:"memoryMb"`
	IdleTimeoutMs  int64  `json:"idleTimeoutMs"`
	MaxAgeMs       int64  `json:"maxAgeMs"`
}

// optionsHash is the hex SHA-256 of the JSON of req's codeSandboxSettings.
func (req *createCodeSandboxRequest) optionsHash() string {
	// Marshalling a struct of strings and ints cannot fail.
	b, _ := json.Marshal(codeSandboxSettings{
		Runtime:        req.Runtime,
		RuntimeVersion: req.RuntimeVersion,
		Slots:          req.Slots,
		MemoryMb:       req.MemoryMb,
		IdleTimeoutMs:  req.IdleTimeoutMs,
		MaxAgeMs:       req.MaxAgeMs,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type codeSandboxResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
	State   string `json:"state"`
}

// codeCreateError is a create failure with the response it gets.
type codeCreateError struct {
	code       int
	msg        string
	reason     string
	retryAfter string
}

func (e *codeCreateError) Error() string { return e.msg }

func writeCodeCreateError(w http.ResponseWriter, err error) {
	var cerr *codeCreateError
	switch {
	case errors.As(err, &cerr):
		if cerr.retryAfter != "" {
			w.Header().Set("Retry-After", cerr.retryAfter)
		}
		writeErrorReason(w, cerr.code, cerr.msg, cerr.reason)
	case errors.Is(err, registry.ErrNoRunners), errors.Is(err, errRunnersAtCapacity):
		writeNoCapacity(w, err)
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// codeCreateResult is what the creates of one name share.
type codeCreateResult struct {
	rec     *store.SandboxRecord
	created bool
}

// handleCreateCodeSandbox handles POST /code/sandboxes: it returns the
// current code sandbox of the name, or creates one. Concurrent creates of a
// name on this pod share one create through flight; across pods the store's
// unique name index decides, and a pod that loses deletes its runner sandbox
// and returns the one that was saved.
func handleCreateCodeSandbox(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, mrec *metrics.APIRecorder, quota *tenantQuota, flight *singleflight.Group) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { mrec.ObserveSandboxOp(metrics.OpCreate, success) }()

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

		var req createCodeSandboxRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCodeCreateBody))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := req.validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Admin keys share the admin pseudo-tenant, which has no quota.
		owner := codeSandboxOwner{tenantID: store.AdminTenantID}
		if authID.Role == roleTenant {
			tenant, err := s.GetTenant(authID.TenantID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tenant == nil {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			owner = codeSandboxOwner{tenantID: tenant.ID, maxSandboxes: tenant.MaxSandboxes}
		}
		tenantID := owner.tenantID

		hash := req.optionsHash()
		// The shared create goes on when this client leaves: the others
		// waiting for it must not fail with it, and a runner create in
		// progress finishes anyway (see runnerCreateBudget).
		shared := r.WithContext(context.WithoutCancel(r.Context()))
		ran := false
		v, err, _ := flight.Do(tenantID+"\x00"+req.Name, func() (any, error) {
			ran = true
			return createCodeSandbox(shared, s, reg, cfg, mrec, quota, owner, &req, hash)
		})
		if err != nil {
			writeCodeCreateError(w, err)
			return
		}
		res := v.(*codeCreateResult)
		obs.FieldsFrom(r.Context()).Add("sandbox_id", res.rec.ID)
		if res.rec.OptionsHash != hash {
			writeErrorReason(w, http.StatusConflict, "sandbox name exists with other settings", reasonSettingsMismatch)
			return
		}
		code := http.StatusOK
		created := ran && res.created
		if created {
			code = http.StatusCreated
		}
		writeJSON(w, code, &codeSandboxResponse{ID: res.rec.ID, Name: res.rec.Name, Created: created, State: "ready"})
		success = true
	}
}

// codeSandboxOwner is the tenant a code sandbox is created for.
type codeSandboxOwner struct {
	tenantID     string
	maxSandboxes int // 0 = unlimited
}

// createCodeSandbox returns the current code sandbox of the name, or creates
// it with req's settings.
func createCodeSandbox(r *http.Request, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, mrec *metrics.APIRecorder, quota *tenantQuota, owner codeSandboxOwner, req *createCodeSandboxRequest, hash string) (*codeCreateResult, error) {
	ctx := r.Context()
	tenantID := owner.tenantID

	existing, err := s.GetCodeSandboxByName(tenantID, req.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !isPastIdleDeleteWindow(existing, cfg, time.Now().Unix()) {
			return &codeCreateResult{rec: existing}, nil
		}
		// Past its idle window the sandbox is as good as gone, and its name
		// would keep pointing to it until the sweeper ran.
		if err := deleteIdleCodeSandbox(ctx, s, reg, cfg, existing.ID); err != nil {
			return nil, err
		}
	}

	releaseQuota, ok, err := quota.reserve(tenantID, owner.maxSandboxes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &codeCreateError{code: http.StatusForbidden, msg: "tenant sandbox quota exceeded"}
	}
	// A create that is not saved frees its slot on return, after any runner
	// rollback; a saved one right after the save, below.
	defer releaseQuota(false)

	sandboxID := generateUUID()
	requested := runnerruntime.CreateOptions{Egress: runnerruntime.EgressNone}
	createOptionsJSON, err := json.Marshal(requested)
	if err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	placed, err := placeSandbox(r, reg, cfg, mrec, sandboxID, string(createOptionsJSON),
		"tenant_id", tenantID, "kind", store.KindCode)
	if err != nil {
		return nil, err
	}
	run, gresp := placed.run, placed.resp
	controlAddr := run.ControlGRPCAddr
	rollbackRunnerCreate := func() {
		deleteCtx, cancelDelete := context.WithTimeout(ctx, runnerCreateBudget)
		delErr := runnerctl.DeleteSandbox(deleteCtx, controlAddr, cfg.RunnerAPIKey, runnerControlTLS(cfg), sandboxID)
		cancelDelete()
		if delErr != nil {
			slog.ErrorContext(ctx, "create code sandbox failed: compensating runner delete, sandbox is untracked",
				"sandbox_id", sandboxID, "runner_id", run.ID, "runner_control_grpc_addr", controlAddr, "error", delErr)
		}
	}

	applied, err := runnerruntime.ParseCreateOptions(gresp.GetAppliedCreateOptionsJson())
	if err != nil || *applied != requested {
		rollbackRunnerCreate()
		slog.ErrorContext(ctx, "create code sandbox failed: runner did not apply the requested egress",
			"sandbox_id", sandboxID, "runner_id", run.ID,
			"applied_create_options_json", gresp.GetAppliedCreateOptionsJson(), "error", err)
		return nil, &codeCreateError{code: http.StatusBadGateway, msg: "runner did not apply the requested egress"}
	}

	record := &store.SandboxRecord{
		ID:                    sandboxID,
		Status:                "running",
		CreatedAt:             now,
		LastActiveAt:          now,
		ContainerIP:           gresp.GetContainerIp(),
		DaemonPort:            8081,
		RunnerID:              run.ID,
		RunnerHTTPBase:        strings.TrimRight(run.HTTPBaseURL, "/"),
		RunnerControlGRPCAddr: controlAddr,
		TenantID:              tenantID,
		Ephemeral:             true,
		Egress:                string(runnerruntime.EgressNone),
		Kind:                  store.KindCode,
		Name:                  req.Name,
		OptionsHash:           hash,
		IdleTimeoutMs:         req.IdleTimeoutMs,
		MaxAgeMs:              req.MaxAgeMs,
	}
	err = s.Create(record)
	if errors.Is(err, store.ErrSandboxNameTaken) {
		// Another API pod saved a sandbox for the name first.
		rollbackRunnerCreate()
		winner, getErr := s.GetCodeSandboxByName(tenantID, req.Name)
		if getErr != nil {
			return nil, getErr
		}
		if winner == nil {
			return nil, &codeCreateError{code: http.StatusServiceUnavailable, msg: "sandbox name changed during create, retry", retryAfter: "1"}
		}
		return &codeCreateResult{rec: winner}, nil
	}
	if err != nil {
		rollbackRunnerCreate()
		// Unless the store refused the row, it may have been saved anyway,
		// say when a commit's reply was lost: count it as saved, the safe side.
		releaseQuota(!errors.Is(err, store.ErrTenantQuotaExceeded) && !errors.Is(err, store.ErrTenantNotFound))
		slog.ErrorContext(ctx, "create code sandbox failed: store record",
			"sandbox_id", sandboxID, "runner_id", run.ID, "error", err)
		switch {
		case errors.Is(err, store.ErrTenantQuotaExceeded):
			return nil, &codeCreateError{code: http.StatusForbidden, msg: "tenant sandbox quota exceeded"}
		case errors.Is(err, store.ErrTenantNotFound):
			return nil, &codeCreateError{code: http.StatusConflict, msg: "tenant not found"}
		}
		return nil, &codeCreateError{code: http.StatusInternalServerError, msg: "failed to store sandbox: " + err.Error()}
	}
	releaseQuota(true)
	slog.InfoContext(ctx, "create code sandbox succeeded", "sandbox_id", sandboxID, "runner_id", run.ID)
	return &codeCreateResult{rec: record, created: true}, nil
}

// deleteIdleCodeSandbox deletes a code sandbox that is past its idle window,
// unless it has been deleted or become active since.
func deleteIdleCodeSandbox(ctx context.Context, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, id string) error {
	ctx, cancel := context.WithTimeout(ctx, runnerCreateBudget)
	defer cancel()
	unlock, err := s.LockSandbox(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := s.Get(id)
	if err != nil {
		return err
	}
	if rec == nil || !isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
		return nil
	}
	if err := runnerctl.DeleteSandbox(ctx, resolveControlAddr(rec, reg), cfg.RunnerAPIKey, runnerControlTLS(cfg), rec.ID); err != nil {
		return &codeCreateError{code: http.StatusBadGateway, msg: "failed to delete idle sandbox: " + err.Error()}
	}
	return s.Delete(rec.ID)
}
