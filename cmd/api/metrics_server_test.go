package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

func TestNewMetricsServerSkipsWhenNotNeeded(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.APIConfig
		rec  *metrics.APIRecorder
	}{
		{
			name: "metrics disabled",
			cfg:  &config.APIConfig{ListenAddr: ":8080", MetricsListenAddr: ":9100"},
			rec:  metrics.NewAPIRecorder(false),
		},
		{
			name: "no metrics address",
			cfg:  &config.APIConfig{ListenAddr: ":8080"},
			rec:  metrics.NewAPIRecorder(true),
		},
		{
			name: "address on the API port",
			cfg:  &config.APIConfig{ListenAddr: ":8080", MetricsListenAddr: ":8080"},
			rec:  metrics.NewAPIRecorder(true),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, lis, err := newMetricsServer(tc.cfg, tc.rec)
			if err != nil {
				t.Fatalf("newMetricsServer: %v", err)
			}
			if srv != nil || lis != nil {
				t.Fatalf("expected no metrics server, got srv=%v lis=%v", srv, lis)
			}
		})
	}
}

func TestNewMetricsServerBindsDedicatedPort(t *testing.T) {
	// Port 0 lets the kernel pick, so the test never races a fixed port.
	cfg := &config.APIConfig{ListenAddr: ":8080", MetricsListenAddr: "127.0.0.1:0"}

	srv, lis, err := newMetricsServer(cfg, metrics.NewAPIRecorder(true))
	if err != nil {
		t.Fatalf("newMetricsServer: %v", err)
	}
	if srv == nil || lis == nil {
		t.Fatal("expected a metrics server for a dedicated port")
	}
	defer lis.Close()

	// Bound at construction, not when serving starts.
	if _, ok := lis.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("expected a bound TCP listener, got %T", lis.Addr())
	}

	for path, want := range map[string]int{"/metrics": http.StatusOK, "/healthz": http.StatusNotFound} {
		rr := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != want {
			t.Errorf("%s: expected %d, got %d", path, want, rr.Code)
		}
	}
}

func TestNewMetricsServerFailsOnOccupiedPort(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer squatter.Close()

	cfg := &config.APIConfig{ListenAddr: ":8080", MetricsListenAddr: squatter.Addr().String()}

	srv, lis, err := newMetricsServer(cfg, metrics.NewAPIRecorder(true))
	if err == nil {
		if lis != nil {
			lis.Close()
		}
		t.Fatal("expected a bind error for an occupied metrics port")
	}
	if srv != nil || lis != nil {
		t.Fatalf("expected nothing back on error, got srv=%v lis=%v", srv, lis)
	}
}
