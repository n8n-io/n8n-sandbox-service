package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// withVolumeObjects serves the local volume store's objects beside the
// authenticated routes in next: a signed URL authorizes these requests in
// place of an API key. Downloads count against a per-scope cap of the same
// size as the per-tenant one, kept apart so they never compete with the
// tenant's own requests.
func withVolumeObjects(next http.Handler, vs *volumestore.Local, maxInflight int, rec *metrics.APIRecorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+volumestore.LocalObjectPath+"{name...}", handleVolumeObject(vs, limits.NewKeyedLimiter(maxInflight), rec))
	var objects http.Handler = mux
	if rec.Enabled() {
		objects = metrics.HTTPMiddleware(rec)(objects)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, volumestore.LocalObjectPath) {
			objects.ServeHTTP(writer, req)
			return
		}
		next.ServeHTTP(writer, req)
	})
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
