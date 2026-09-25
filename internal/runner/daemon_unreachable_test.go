package runner

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

// Both 503 paths used to drop the transport error, leaving a client's "daemon
// temporarily unavailable" with nothing on the runner to explain it. The line
// has to carry the sandbox id, so the runtime's own diagnostics for that sandbox
// can be found, and the trace id, so it joins the request event.
func TestProxyHandlersLogWhyTheDaemonWasUnreachable(t *testing.T) {
	// A closed listener's address refuses connections, which is the transport
	// error the daemon proxy produces when the guest cannot be dialed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	daemonURL := "http://" + listener.Addr().String()
	_ = listener.Close()

	for name, newHandler := range wakingHandlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(obs.TraceHandler(slog.NewJSONHandler(&buf, nil))))
			t.Cleanup(func() { slog.SetDefault(previous) })

			handler := newHandler(&fakeRuntime{daemonURL: daemonURL}, proxyTestConfig(), metrics.NewRunnerRecorder(false))
			traceparent := obs.NewTraceparent()
			req := proxyTestRequest()
			req = req.WithContext(obs.WithTraceparent(req.Context(), traceparent))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
			}

			var line map[string]any
			for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
				var event map[string]any
				if json.Unmarshal(raw, &event) != nil {
					continue
				}
				if msg, _ := event["msg"].(string); strings.HasSuffix(msg, ": daemon request failed") {
					line = event
				}
			}
			if line == nil {
				t.Fatalf("no daemon request failed event logged; got:\n%s", buf.String())
			}
			if id := line["sandbox_id"]; id != proxyTestSandboxID {
				t.Errorf("sandbox_id = %v, want %s", id, proxyTestSandboxID)
			}
			if tid := line["trace_id"]; tid != obs.TraceIDOf(traceparent) {
				t.Errorf("trace_id = %v, want %s", tid, obs.TraceIDOf(traceparent))
			}
			if errText, _ := line["err"].(string); !strings.Contains(errText, "connection refused") {
				t.Errorf("err = %q, want the transport error", errText)
			}
		})
	}
}
