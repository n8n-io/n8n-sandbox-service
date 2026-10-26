package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// lostContainerRuntime is a proxy runtime whose container can vanish under the
// runner, as `docker rm -f` inside the runner does.
type lostContainerRuntime struct {
	*proxyRuntime
	gone    atomic.Bool
	creates atomic.Int32
	// hold, when set, keeps CreateSandbox from returning until it is closed.
	hold    chan struct{}
	started chan struct{}
}

func (l *lostContainerRuntime) CreateSandbox(ctx context.Context, id string, opts *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	l.creates.Add(1)
	if l.hold != nil {
		close(l.started)
		<-l.hold
	}
	info, err := l.proxyRuntime.CreateSandbox(ctx, id, opts)
	l.gone.Store(false)
	return info, err
}

func (l *lostContainerRuntime) GetSandboxInfo(_ context.Context, id string) (*runnerruntime.SandboxInfo, error) {
	if l.gone.Load() {
		return nil, runnerruntime.ErrSandboxNotFound
	}
	return &runnerruntime.SandboxInfo{ID: id, IP: "10.0.0.2"}, nil
}

func newLostContainerControl(t *testing.T) (*SandboxControlGRPC, *lostContainerRuntime) {
	t.Helper()
	ctl, prt, _ := newProxyControl(t, true)
	rt := &lostContainerRuntime{proxyRuntime: prt}
	ctl.Runtime = rt
	return ctl, rt
}

func runnerCtx() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
}

// A container removed behind the runner's back leaves the egress proxy's
// record: a get reports the sandbox gone, and a create under its id replaces
// the stale record rather than refusing it as AlreadyExists.
func TestCreateReplacesRecordWhoseContainerIsGone(t *testing.T) {
	ctl, rt := newLostContainerControl(t)
	ctx := runnerCtx()
	const id = "11111111-1111-4111-8111-111111111111"
	if _, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rt.gone.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes/{id}", GetSandbox(rt))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sandboxes/"+id, nil))
	if rec.Code != http.StatusNotFound || !sandboxproxy.RunnerReportsSandboxGone(rec.Result()) {
		t.Fatalf("get of a sandbox whose container is gone = %d gone=%v", rec.Code, sandboxproxy.RunnerReportsSandboxGone(rec.Result()))
	}

	resp, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts})
	if err != nil {
		t.Fatalf("re-create after the container was lost: %v", err)
	}
	if resp.GetSandboxId() != id || rt.creates.Load() != 2 || !ctl.Egress.Has(id) {
		t.Fatalf("re-create: resp %v, creates %d, policy %v", resp, rt.creates.Load(), ctl.Egress.Has(id))
	}

	// The container is back, so a third create is a live sandbox and still refused.
	if _, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("create over a live sandbox: %v", err)
	}
	if rt.creates.Load() != 2 || !ctl.Egress.Has(id) {
		t.Fatalf("refused create touched the live sandbox: creates %d, policy %v", rt.creates.Load(), ctl.Egress.Has(id))
	}
}

// A create still in flight has its record installed before its container
// exists; a second create for the id must not take that for a stale record.
func TestCreateRefusesWhileCreateInFlight(t *testing.T) {
	ctl, rt := newLostContainerControl(t)
	rt.gone.Store(true)
	rt.hold, rt.started = make(chan struct{}), make(chan struct{})
	ctx := runnerCtx()
	const id = "11111111-1111-4111-8111-111111111111"

	var wg sync.WaitGroup
	var firstErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, firstErr = ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts})
	}()
	<-rt.started
	_, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts})
	close(rt.hold)
	wg.Wait()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("second create during the first: %v", err)
	}
	if firstErr != nil || !ctl.Egress.Has(id) || rt.creates.Load() != 1 {
		t.Fatalf("first create: %v, policy %v, creates %d", firstErr, ctl.Egress.Has(id), rt.creates.Load())
	}
}
