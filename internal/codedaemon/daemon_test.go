package codedaemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDaemonHealth(t *testing.T) {
	d := New(Config{})
	for addr, want := range map[string]int{"127.0.0.1:123": 403, "[::1]:123": 403, "invalid": 403, "192.0.2.1:123": 200} {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("health from %s: status = %d, want %d", addr, w.Code, want)
		}
	}
}

func TestDaemonShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(Config{AllowLocalClients: true}).serve(ctx, listener) }()
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
