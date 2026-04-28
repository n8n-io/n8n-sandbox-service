package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

var sandboxPathRe = regexp.MustCompile(`/var/sandboxes/[0-9a-f-]+/(?:rootfs|merged|upper|work|socket)`)

// APIError is the JSON body returned for error responses.
type APIError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// sanitizeError strips internal filesystem paths from error messages so they
// are not leaked to API callers.
func sanitizeError(msg string) string {
	// Strip sandbox-internal filesystem prefixes.
	msg = sandboxPathRe.ReplaceAllString(msg, "")
	// Replace /sandbox/ prefix with /
	msg = strings.ReplaceAll(msg, "/sandbox/", "/")
	return msg
}

func writeError(w http.ResponseWriter, code int, msg string) {
	sanitized := sanitizeError(msg)
	if sanitized != msg {
		slog.Debug("sanitized error message", "original", msg, "sanitized", sanitized)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(APIError{Error: sanitized, Code: code}); err != nil {
		slog.Warn("write error response", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json response", "err", err)
	}
}
