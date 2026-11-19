package firecracker

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// A proxy sandbox gets a listener in its slot, which names it to the proxy
// whatever connects; teardown closes it.
func TestProxySandboxGetsAListenerInItsSlot(t *testing.T) {
	rt := testRuntime(1)
	rt.config.EgressProxyPort = 3128
	rt.deps.run = func(context.Context, string, ...string) error { return nil }
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return &fakeProcess{}, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.cloneRootfs = func(context.Context, string, string) error { return nil }
	rt.deps.cloneGoldenSnapshot = func(context.Context, string, string, string) error { return nil }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return &fakeProxy{}, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return nil }
	var listenedIn, listenedOn string
	var ln net.Listener
	rt.deps.listenEgress = func(netnsPath, network, addr string) (net.Listener, error) {
		listenedIn, listenedOn = netnsPath, addr
		var err error
		ln, err = net.Listen(network, "127.0.0.1:0")
		return ln, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = rt.ServeEgressProxy(ctx, func(identify func(string) (string, bool)) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, _ := identify(r.RemoteAddr)
				_, _ = io.WriteString(w, id)
			})
		})
	}()
	for rt.egressHandler.Load() == nil {
	}

	const id = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), id, &runnerruntime.CreateOptions{Egress: runnerruntime.EgressProxy}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if listenedIn != "/run/netns/fc-sb-0" || listenedOn != "172.16.0.1:3128" {
		t.Fatalf("listened in %q on %q", listenedIn, listenedOn)
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != id {
		t.Fatalf("identified as %q", body)
	}
	if u, ok := rt.EgressProxyURL(id); !ok || u != "http://172.16.0.1:3128" {
		t.Fatalf("proxy url %q %v", u, ok)
	}
	if err := rt.DeleteSandbox(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Fatal("listener survived delete")
	}
}
