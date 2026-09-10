package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// TestSandboxProxyRefusesRunnerRedirect: a runner answering a proxied route
// with a 3xx must not have that response relayed, or a compromised runner could
// send the client, and the API key it holds, to a host of its choosing.
func TestSandboxProxyRefusesRunnerRedirect(t *testing.T) {
	logs := captureLogs(t)
	var (
		status       atomic.Int32
		upstreamHits atomic.Int64
	)
	// Long enough that logging it uncapped would be visible in the event.
	location := "https://attacker.example/" + strings.Repeat("a", 4096)
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Location", location)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("moved"))
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	for i, code := range []int{301, 302, 303, 307, 308} {
		status.Store(int32(code))
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadGateway {
			t.Errorf("runner %d: expected %d, got %d body=%s", code, http.StatusBadGateway, rr.Code, rr.Body.String())
		}
		if loc := rr.Header().Get("Location"); loc != "" {
			t.Errorf("runner %d: Location relayed: %q", code, loc)
		}
		var body APIError
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != errRunnerRedirect.Error() {
			t.Errorf("runner %d: unexpected body %s (err=%v)", code, rr.Body.String(), err)
		}
		if hits := upstreamHits.Load(); hits != int64(i+1) {
			t.Fatalf("runner %d: expected %d upstream requests, got %d", code, i+1, hits)
		}
	}

	// The refusal must not count as activity, or a retrying client would keep
	// the idle sweeper from ever reclaiming a sandbox on a misbehaving runner.
	rec, err := s.Get(sid)
	if err != nil || rec == nil {
		t.Fatalf("get sandbox: rec=%v err=%v", rec, err)
	}
	if rec.LastActiveAt != 1 {
		t.Fatalf("LastActiveAt = %d, want 1 unchanged", rec.LastActiveAt)
	}

	// The runner chooses the Location, so the log keeps only a bounded prefix.
	logged := 0
	for _, event := range logs() {
		loc, ok := event["runner_redirect_location"].(string)
		if !ok {
			continue
		}
		logged++
		if len(loc) != 200 {
			t.Fatalf("logged runner_redirect_location has %d bytes, want capped at 200", len(loc))
		}
	}
	if logged != int(upstreamHits.Load()) {
		t.Fatalf("expected %d events with runner_redirect_location, got %d", upstreamHits.Load(), logged)
	}
}
