package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// Two runners, one serving only the default and one also serving python: a
// python create lands on the second, a default one on the emptier first, an
// uncatalogued template is 400, and a catalogued one no runner serves is 503.
func TestCreateSandboxPlacesByTemplate(t *testing.T) {
	plain, python := &fakeSandboxControl{}, &fakeSandboxControl{}
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.DefaultMaxSandboxes = 50
	cfg.HeartbeatGrace = time.Minute
	cfg.Templates = []string{"default", "python", "go"}
	reg := registry.New(time.Minute)
	reg.UpsertRunner(registry.Runner{ID: "plain", HTTPBaseURL: "https://127.0.0.1:9", ControlGRPCAddr: startFakeRunnerControl(t, plain), Healthy: true, CapacityTotal: 10})
	reg.UpsertRunner(registry.Runner{ID: "py", HTTPBaseURL: "https://127.0.0.1:9", ControlGRPCAddr: startFakeRunnerControl(t, python), Healthy: true, CapacityTotal: 10, CapacityUsed: 5,
		Templates: []registry.Template{{Name: "default"}, {Name: "python", VCPUs: 4, MemoryMB: 8192, DiskMB: 20480}}})
	reg.UpsertRunner(registry.Runner{ID: "drained", HTTPBaseURL: "https://127.0.0.1:9", ControlGRPCAddr: "127.0.0.1:1", Healthy: true, CapacityTotal: 10, Cordoned: true,
		Templates: []registry.Template{{Name: "python"}}})
	router, err := NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatal(err)
	}

	if rr := postCreateSandbox(t, router, "admin-key", `{"template":"python"}`); rr.Code != http.StatusCreated {
		t.Fatalf("python: %d %s", rr.Code, rr.Body.String())
	}
	if len(python.createOptionsJSON) != 1 || python.createOptionsJSON[0] != `{"egress":"public","template":"python"}` || len(plain.createOptionsJSON) != 0 {
		t.Fatalf("python creates: plain %v, python %v", plain.createOptionsJSON, python.createOptionsJSON)
	}
	if rr := postCreateSandbox(t, router, "admin-key", `{}`); rr.Code != http.StatusCreated || len(plain.createOptionsJSON) != 1 {
		t.Fatalf("default: %d %s, plain %v", rr.Code, rr.Body.String(), plain.createOptionsJSON)
	}
	rr := postCreateSandbox(t, router, "admin-key", `{"template":"rust"}`)
	var body APIError
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusBadRequest || body.Reason != reasonUnknownTemplate {
		t.Fatalf("uncatalogued: %d %s", rr.Code, rr.Body.String())
	}
	rr = postCreateSandbox(t, router, "admin-key", `{"template":"go"}`)
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusServiceUnavailable || body.Reason != reasonNoCapacity || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("no runner serves go: %d %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, router, http.MethodGet, "/templates", "admin-key", "")
	var list listTemplatesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("GET /templates: %d %s", rr.Code, rr.Body.String())
	}
	want := []templateResponse{
		{Name: "default", Runners: 2},
		{Name: "python", VCPUs: 4, MemoryMB: 8192, DiskMB: 20480, Runners: 1},
		{Name: "go"},
	}
	if len(list.Templates) != len(want) {
		t.Fatalf("templates = %+v", list.Templates)
	}
	for i := range want {
		if list.Templates[i] != want[i] {
			t.Fatalf("templates[%d] = %+v, want %+v", i, list.Templates[i], want[i])
		}
	}
}
