package firecracker

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A guest dial failure is the only runner-side trace of a sandbox whose daemon
// cannot be reached, and the netns name in it identifies a slot, which changes
// hands. The line has to name the sandbox, or an incident cannot be traced back
// to the sandbox a client reported.
func TestDaemonProxyLogsSandboxIDWhenGuestDialFails(t *testing.T) {
	events := captureLifecycleEvents(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &tcpDaemonProxy{
		sandboxID: "sandbox-id-123456",
		listener:  listener,
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		dial: func(context.Context, string, string, string) (net.Conn, error) {
			return nil, errors.New("open /run/netns/fc-sb-2: no such file or directory")
		},
	}
	go p.serve("fc-sb-2", "172.16.0.10:8081")

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	// The proxy answers a failed guest dial by closing the connection without a
	// byte, so EOF is what tells us the handler has run.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read = %v, want EOF from the proxy closing the connection", err)
	}
	// Stop waits for the handler goroutine, which is what makes its log write
	// visible here without a race.
	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got := events("firecracker daemon proxy guest dial failed")
	if len(got) != 1 {
		t.Fatalf("guest dial failure events = %d, want 1", len(got))
	}
	if id := got[0]["sandbox_id"]; id != "sandbox-id-123456" {
		t.Errorf("sandbox_id = %v, want sandbox-id-123456", id)
	}
	if netns := got[0]["netns"]; netns != "fc-sb-2" {
		t.Errorf("netns = %v, want fc-sb-2", netns)
	}
	if errText, _ := got[0]["err"].(string); errText == "" {
		t.Error("err is missing from the event")
	}
}
