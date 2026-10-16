package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

func newRunnersGateway(t *testing.T) (http.Handler, *registry.MemoryRegistry) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	reg := registry.New(45 * time.Second)
	router, err := NewGatewayRouter(s, withRunnerTLS(&config.APIConfig{
		APIKeys:             map[string]struct{}{provAdminKey: {}},
		ProvisionerKeys:     map[string]struct{}{provKey: {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: provMaxDflt,
	}), reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, reg
}

func TestListRunnersAsAdmin(t *testing.T) {
	router, reg := newRunnersGateway(t)
	reg.Upsert("r-b", "https://runner-b.internal:8443", "runner-b.internal:9091", false, 10, 4, 1)
	reg.Upsert("r-a", "https://runner-a.internal:8443", "runner-a.internal:9091", true, 8, 2, 0)

	rr := doJSON(t, router, http.MethodGet, "/admin/runners", provAdminKey, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "runner-a.internal") {
		t.Fatalf("response leaks a runner address: %s", rr.Body.String())
	}
	var got listRunnersResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runners) != 2 {
		t.Fatalf("expected 2 runners, got %+v", got.Runners)
	}
	a, b := got.Runners[0], got.Runners[1]
	if a.ID != "r-a" || !a.Healthy || a.CapacityTotal != 8 || a.CapacityUsed != 2 || a.CapacityStopped != 0 {
		t.Fatalf("r-a = %+v", a)
	}
	if b.ID != "r-b" || b.Healthy || b.CapacityTotal != 10 || b.CapacityUsed != 4 || b.CapacityStopped != 1 {
		t.Fatalf("r-b = %+v", b)
	}
	if age := time.Now().Unix() - a.LastSeen; age < 0 || age > 5 {
		t.Fatalf("r-a last_seen = %d, want about now", a.LastSeen)
	}
}

func TestListRunnersEmptyIsArray(t *testing.T) {
	router, _ := newRunnersGateway(t)
	rr := doJSON(t, router, http.MethodGet, "/admin/runners", provAdminKey, "")
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != `{"runners":[]}` {
		t.Fatalf("expected 200 {\"runners\":[]}, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestListRunnersRejectsProvisionerAndTenantKeys(t *testing.T) {
	router, _ := newRunnersGateway(t)
	created := provisionerCreateTenant(t, router, `{"name":"inst","external_ref":"inst-1"}`)
	if created.Key == nil {
		t.Fatal("expected a tenant key")
	}

	for name, key := range map[string]string{"provisioner": provKey, "tenant": created.Key.APIKey} {
		rr := doJSON(t, router, http.MethodGet, "/admin/runners", key, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s key: expected 403, got %d body=%s", name, rr.Code, rr.Body.String())
		}
	}
}
