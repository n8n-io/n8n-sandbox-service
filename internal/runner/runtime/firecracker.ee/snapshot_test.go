package firecracker

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
)

// Without clock_realtime a restored guest keeps the wall clock it had when the
// snapshot was taken, so every new sandbox runs behind by the golden snapshot's age.
func TestLoadSnapshotAdvancesTheGuestClock(t *testing.T) {
	type request struct {
		method string
		path   string
		body   string
	}
	var mu sync.Mutex
	var got []request
	socket := listenUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s body: %v", r.URL.Path, err)
		}
		mu.Lock()
		got = append(got, request{method: r.Method, path: r.URL.Path, body: string(raw)})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := loadSnapshot(context.Background(), socket, testConfig()); err != nil {
		t.Fatalf("loadSnapshot() failed: %v", err)
	}

	want := request{
		method: http.MethodPut,
		path:   "/snapshot/load",
		body:   `{"snapshot_path":"/snapshot_state","mem_backend":{"backend_type":"File","backend_path":"/snapshot_mem"},"track_dirty_pages":false,"resume_vm":true,"clock_realtime":true}`,
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("loadSnapshot() sent %d requests, want 1: %+v", len(got), got)
	}
	if got[0] != want {
		t.Errorf("request = %+v, want %+v", got[0], want)
	}
}
