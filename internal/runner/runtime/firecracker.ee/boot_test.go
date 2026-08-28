package firecracker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// listenUnixHTTP serves handler on a Unix socket and returns its path, which is
// what Firecracker's API is. The directory is deliberately not t.TempDir(): a
// socket path is capped near 104 bytes and test names make TempDir paths long.
func listenUnixHTTP(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

// coldBoot is a transcription of the requests create-golden-snapshot.sh boots the
// golden snapshot with, and the two cannot share code: the script runs standalone
// on runner VMs, ahead of the release carrying this binary. So the wire is the
// contract, and this test is what holds the transcription to it.
func TestColdBootSendsTheGoldenBuildsBootRequests(t *testing.T) {
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

	params := testBootParams(testConfig())
	params.VCPUCount = 2
	params.MemSizeMiB = 2048
	if err := coldBoot(context.Background(), socket, &params); err != nil {
		t.Fatalf("coldBoot() failed: %v", err)
	}

	// InstanceStart last is the part of this that is ordering and not just content:
	// everything before it only configures the VMM.
	want := []request{
		{http.MethodPut, "/machine-config", `{"vcpu_count":2,"mem_size_mib":2048,"smt":false}`},
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinux","boot_args":"` + params.BootArgs + `"}`},
		{http.MethodPut, "/drives/rootfs", `{"drive_id":"rootfs","path_on_host":"/rootfs.ext4","is_root_device":true,"is_read_only":false}`},
		{http.MethodPut, "/network-interfaces/eth0", `{"iface_id":"eth0","guest_mac":"AA:FC:00:00:00:01","host_dev_name":"fc-tap-0"}`},
		{http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`},
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("coldBoot() sent %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("request %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestColdBootWithoutBootParametersFails(t *testing.T) {
	socket := listenUnixHTTP(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("coldBoot reached the Firecracker API with no boot parameters to send")
	}))
	if err := coldBoot(context.Background(), socket, nil); err == nil {
		t.Fatal("expected coldBoot() to fail without boot parameters")
	}
}
