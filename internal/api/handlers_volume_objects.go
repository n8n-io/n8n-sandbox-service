package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// NewVolumeObjectsRouter returns the handler for the listener that serves the
// local volume store's objects to runners, and nothing else. The caller puts
// mutual TLS in front of it; no route takes an API key. A signed URL limits a
// runner to the objects it was handed. Downloads count against a per-scope
// cap of the same size as the per-tenant one on the public listener.
func NewVolumeObjectsRouter(vs *volumestore.Local, maxInflight int, rec *metrics.APIRecorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+volumestore.LocalObjectPath+"{name...}", handleVolumeObject(vs, limits.NewKeyedLimiter(maxInflight), rec))
	var handler http.Handler = mux
	if rec.Enabled() {
		handler = metrics.HTTPMiddleware(rec)(handler)
	}
	return RecoveryMiddleware(LoggingMiddleware(handler))
}

// volumeObjectStallTimeout ends a download whose client has stopped reading,
// so that it gives back its slot. The server has no write timeout, which a
// large download would outlast.
var volumeObjectStallTimeout = time.Minute

// stallWriter pushes the write deadline forward before every write.
type stallWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (s stallWriter) Write(buf []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(volumeObjectStallTimeout))
	return s.ResponseWriter.Write(buf)
}

func handleVolumeObject(vs *volumestore.Local, limiter *limits.KeyedLimiter, rec *metrics.APIRecorder) http.HandlerFunc {
	return func(writer http.ResponseWriter, req *http.Request) {
		key, err := vs.VerifyRequest(req)
		if err != nil {
			writeError(writer, http.StatusForbidden, "invalid or expired signature")
			return
		}
		if !limiter.TryAcquire(key.Scope) {
			rec.ObserveRequestLimited(metrics.LimitTenant)
			writer.Header().Set("Retry-After", limitRetryAfter)
			writeErrorReason(writer, http.StatusTooManyRequests, "too many requests in progress for this tenant", reasonTenantRequestLimit)
			return
		}
		defer limiter.Release(key.Scope)
		sw := stallWriter{ResponseWriter: writer, rc: http.NewResponseController(writer)}
		if err := vs.ServeObject(sw, req, key); errors.Is(err, volumestore.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "volume object not found")
		} else if err != nil {
			slog.Error("serve volume object", "scope", key.Scope, "id", key.ID, "kind", key.Kind, "error", err)
			writeError(writer, http.StatusInternalServerError, "failed to read volume object")
		}
	}
}
