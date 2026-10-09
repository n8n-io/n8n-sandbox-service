package api

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// scalerForwardTimeout bounds the forwarded GET .../policy call. Fixed, not
// configurable.
const scalerForwardTimeout = 5 * time.Second

// scalerMaxBodyBytes caps the relayed policy body. The real one is well under 1 KiB.
const scalerMaxBodyBytes = 1 << 20

// handleGetScaler forwards to cfg.ScalerURL + "/policy". On a 200 it relays the
// body and Content-Type; any other outcome is reported as 503.
func handleGetScaler(cfg *config.APIConfig) http.HandlerFunc {
	client := &http.Client{Timeout: scalerForwardTimeout, CheckRedirect: runnerruntime.RefuseRedirect}
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		if cfg.ScalerURL == "" {
			writeError(w, http.StatusServiceUnavailable, "scaler not configured")
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.ScalerURL+"/policy", nil)
		if err != nil {
			slog.ErrorContext(r.Context(), "scaler proxy: build request", "error", err)
			writeError(w, http.StatusServiceUnavailable, "scaler unavailable")
			return
		}
		// The scaler authenticates its own callers. The admin's X-Api-Key stays here.
		if auth := r.Header.Get("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.WarnContext(r.Context(), "scaler proxy: request failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "scaler unavailable")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.WarnContext(r.Context(), "scaler proxy: unexpected status", "status", resp.StatusCode)
			writeError(w, http.StatusServiceUnavailable, "scaler unavailable")
			return
		}

		// Read the whole body before replying: the client timeout also covers the
		// body, so a slow scaler must fail as a 503, not as a truncated 200.
		body, err := io.ReadAll(io.LimitReader(resp.Body, scalerMaxBodyBytes+1))
		if err != nil || len(body) > scalerMaxBodyBytes {
			slog.WarnContext(r.Context(), "scaler proxy: read body", "error", err, "bytes", len(body))
			writeError(w, http.StatusServiceUnavailable, "scaler unavailable")
			return
		}

		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
