package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/obs"
)

// captureLogs redirects the default logger into a buffer for the duration of
// the test and returns the events it recorded.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return func() []map[string]any {
		var events []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("log line is not JSON: %v", err)
			}
			events = append(events, event)
		}
		return events
	}
}

func TestLoggingMiddlewareEmitsCanonicalEvent(t *testing.T) {
	logs := captureLogs(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands in for the handlers and middleware that contribute fields
		// from a request context the outer middleware never sees.
		obs.FieldsFrom(r.Context()).Add("sandbox_id", "sandbox-1")
		w.WriteHeader(http.StatusAccepted)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/sandboxes/sandbox-1/executions?wait=1", nil))

	events := logs()
	if len(events) != 1 {
		t.Fatalf("got %d log events, want exactly one canonical event", len(events))
	}
	event := events[0]
	for field, want := range map[string]any{
		"msg":        "request",
		"method":     http.MethodPost,
		"path":       "/sandboxes/sandbox-1/executions",
		"query":      "wait=1",
		"status":     float64(http.StatusAccepted),
		"sandbox_id": "sandbox-1",
	} {
		if event[field] != want {
			t.Fatalf("event[%q] = %v, want %v", field, event[field], want)
		}
	}
	if _, ok := event["duration_ms"]; !ok {
		t.Fatalf("event is missing duration_ms: %v", event)
	}
	if traceID, _ := event["trace_id"].(string); len(traceID) != 32 {
		t.Fatalf("event trace_id = %q, want a 32 hex character id", traceID)
	}
}

func TestLoggingMiddlewareAdoptsInboundTraceparent(t *testing.T) {
	logs := captureLogs(t)

	const inbound = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var forwarded string
	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = obs.Traceparent(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	req.Header.Set(obs.HeaderTraceparent, inbound)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if forwarded != inbound {
		t.Fatalf("context traceparent = %q, want the inbound header %q", forwarded, inbound)
	}
	if got := logs()[0]["trace_id"]; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("event trace_id = %v, want the inbound trace id", got)
	}
}

func TestLoggingMiddlewareReplacesMalformedTraceparent(t *testing.T) {
	captureLogs(t)

	var forwarded string
	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = obs.Traceparent(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	req.Header.Set(obs.HeaderTraceparent, "not-a-traceparent")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if obs.TraceIDOf(forwarded) == "" {
		t.Fatalf("context traceparent = %q, want a freshly minted one", forwarded)
	}
}

func TestLoggingMiddlewareSkipsProbes(t *testing.T) {
	logs := captureLogs(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, path := range []string{"/healthz", "/metrics"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if events := logs(); len(events) != 0 {
		t.Fatalf("got %d log events for probe endpoints, want none", len(events))
	}
}
