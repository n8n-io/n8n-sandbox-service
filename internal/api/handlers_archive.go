package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// writeRestoring answers a request for a sandbox whose restore runs.
func writeRestoring(w http.ResponseWriter) {
	w.Header().Set("Retry-After", restoreRetryAfter)
	writeErrorReason(w, http.StatusServiceUnavailable, "sandbox is being restored from its archive", reasonRestoring)
}

// lockedSandbox loads id under its lock for a lifecycle call, answering 404
// for a sandbox the caller cannot see.
func lockedSandbox(w http.ResponseWriter, r *http.Request, s store.SandboxStore, cfg *config.APIConfig) (*store.SandboxRecord, func(), bool) {
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
	return rec, unlock, true
}

// stopForArchive stops a running sandbox so it can be archived. The runner
// refuses while a run is in progress.
func stopForArchive(r *http.Request, s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, rec *store.SandboxRecord) error {
	ctx, cancel := context.WithTimeout(r.Context(), runnerLifecycleBudget)
	defer cancel()
	if err := runnerctl.StopSandbox(ctx, resolveControlAddr(rec, reg), cfg.RunnerAPIKey, runnerControlTLS(cfg), rec.ID); err != nil {
		return err
	}
	if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
		return err
	}
	rec.Status = "stopped"
	return nil
}

// handleArchiveSandbox archives a sandbox now: stopped first if it runs, and
// answered once the archive is recorded.
func handleArchiveSandbox(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, arch *archiver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if arch == nil {
			writeErrorReason(w, http.StatusNotImplemented, "no archive store is configured", reasonNotImplemented)
			return
		}
		rec, unlock, ok := lockedSandbox(w, r, s, cfg)
		if !ok {
			return
		}
		defer unlock()
		switch {
		case rec.Ephemeral:
			writeErrorReason(w, http.StatusConflict, "ephemeral sandboxes are never archived", reasonEphemeral)
			return
		case rec.Status == statusArchived:
			writeJSON(w, http.StatusOK, sandboxResponse(rec))
			return
		case rec.Status == statusRestoring:
			writeRestoring(w)
			return
		case rec.Status == "running":
			if err := stopForArchive(r, s, reg, cfg, rec); err != nil {
				if status.Code(err) == codes.FailedPrecondition {
					writeErrorReason(w, http.StatusConflict, "an execution is running or streaming", reasonSandboxBusy)
					return
				}
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusServiceUnavailable, "runner unavailable")
				return
			}
		}
		if err := arch.archive(r.Context(), rec); err != nil {
			var rpcErr *runnerCallError
			if errors.As(err, &rpcErr) && status.Code(rpcErr.err) == codes.FailedPrecondition {
				writeErrorReason(w, http.StatusConflict, "an execution is running or streaming", reasonSandboxBusy)
				return
			}
			slog.WarnContext(r.Context(), "archive sandbox", "sandbox_id", rec.ID, "err", err)
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "archive failed; the sandbox is still stopped on its runner")
			return
		}
		fresh, err := s.Get(rec.ID)
		if err != nil || fresh == nil {
			writeError(w, http.StatusInternalServerError, "archived sandbox not readable")
			return
		}
		writeJSON(w, http.StatusOK, sandboxResponse(fresh))
	}
}

// handleRestoreSandbox starts a restore ahead of use.
func handleRestoreSandbox(s store.SandboxStore, cfg *config.APIConfig, arch *archiver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, unlock, ok := lockedSandbox(w, r, s, cfg)
		if !ok {
			return
		}
		unlock()
		if rec.Status != statusArchived && rec.Status != statusRestoring {
			writeJSON(w, http.StatusOK, sandboxResponse(rec))
			return
		}
		if arch != nil {
			arch.startRestore(rec)
		}
		writeRestoring(w)
	}
}
