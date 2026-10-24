package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const reasonUnknownSecretRef = "unknown_secret_ref"

// egressCallBudget bounds the runner call of an egress change.
const egressCallBudget = 30 * time.Second

// proxySandbox loads id for an egress change and answers for it when it
// cannot take one: 404 not found or not the caller's, 409 not a proxy sandbox.
func proxySandbox(w http.ResponseWriter, r *http.Request, s store.SandboxStore, cfg *config.APIConfig) (*store.SandboxRecord, func(), bool) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid sandbox id")
		return nil, nil, false
	}
	unlock, err := s.LockSandbox(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	rec, err := s.Get(id)
	if err != nil {
		unlock()
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if rec == nil || !canAccessSandbox(r, rec) || isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
		unlock()
		writeError(w, http.StatusNotFound, "sandbox not found")
		return nil, nil, false
	}
	if rec.Egress != string(runnerruntime.EgressProxy) {
		unlock()
		writeErrorReason(w, http.StatusConflict, "sandbox egress is "+rec.Egress+", not proxy", reasonEgressMismatch)
		return nil, nil, false
	}
	return rec, unlock, true
}

// writeRunnerEgressError answers a runner call that failed.
func writeRunnerEgressError(w http.ResponseWriter, r *http.Request, rec *store.SandboxRecord, err error) {
	switch status.Code(err) {
	case codes.FailedPrecondition:
		writeErrorReason(w, http.StatusBadRequest, "egress_proxy names a secret_ref the sandbox's tenant does not have", reasonUnknownSecretRef)
	case codes.NotFound:
		writeError(w, http.StatusNotFound, "sandbox not found on its runner")
	case codes.Unimplemented:
		writeErrorReason(w, http.StatusNotImplemented, "the sandbox's runner cannot serve this egress_proxy", reasonNotImplemented)
	default:
		slog.WarnContext(r.Context(), "runner egress call failed", "sandbox_id", rec.ID, "runner_id", rec.RunnerID, "err", err)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
	}
}

// handlePutEgressProxy replaces a proxy sandbox's spec: on its runner first,
// then in the store, so the stored spec is never one the runner refused.
func handlePutEgressProxy(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig) http.HandlerFunc {
	tlsCfg := runnerControlTLS(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCreateBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		spec, err := parseEgressProxySpec(bytes.TrimSpace(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		canonical, err := json.Marshal(&spec)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rec, unlock, ok := proxySandbox(w, r, s, cfg)
		if !ok {
			return
		}
		defer unlock()
		ctx, cancel := context.WithTimeout(r.Context(), egressCallBudget)
		defer cancel()
		if err := runnerctl.UpdateEgressProxy(ctx, resolveControlAddr(rec, reg), cfg.RunnerAPIKey, tlsCfg, rec.ID, string(canonical)); err != nil {
			writeRunnerEgressError(w, r, rec, err)
			return
		}
		if err := s.UpdateEgressProxy(rec.ID, string(canonical)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.InfoContext(r.Context(), "sandbox egress_proxy replaced", "sandbox_id", rec.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}

type modelEgress struct {
	Blocked *bool `json:"blocked"`
}

type modelEgressResponse struct {
	Blocked bool `json:"blocked"`
}

func handleGetModelEgress(s store.SandboxStore, cfg *config.APIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, unlock, ok := proxySandbox(w, r, s, cfg)
		if !ok {
			return
		}
		unlock()
		writeJSON(w, http.StatusOK, modelEgressResponse{Blocked: rec.ModelBlocked})
	}
}

// handlePutModelEgress blocks or unblocks the sandbox's model requests: on the
// runner, which enforces it, then in the store.
func handlePutModelEgress(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig) http.HandlerFunc {
	tlsCfg := runnerControlTLS(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		var req modelEgress
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.Blocked == nil {
			writeError(w, http.StatusBadRequest, `body must be {"blocked": true|false}`)
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			writeError(w, http.StatusBadRequest, `body must be {"blocked": true|false}`)
			return
		}
		rec, unlock, ok := proxySandbox(w, r, s, cfg)
		if !ok {
			return
		}
		defer unlock()
		ctx, cancel := context.WithTimeout(r.Context(), egressCallBudget)
		defer cancel()
		if err := runnerctl.SetModelBlocked(ctx, resolveControlAddr(rec, reg), cfg.RunnerAPIKey, tlsCfg, rec.ID, *req.Blocked); err != nil {
			writeRunnerEgressError(w, r, rec, err)
			return
		}
		if err := s.SetModelBlocked(rec.ID, *req.Blocked); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.InfoContext(r.Context(), "sandbox model egress changed", "sandbox_id", rec.ID, "blocked", *req.Blocked)
		writeJSON(w, http.StatusOK, modelEgressResponse{Blocked: *req.Blocked})
	}
}
