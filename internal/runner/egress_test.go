package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/egressproxy"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/credentials"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// proxyRuntime is a runtime that can host the egress proxy, with a daemon
// that records the files written to it.
type proxyRuntime struct {
	createOptsRuntime
	mu      sync.Mutex
	files   map[string]string
	deleted []string
}

func (p *proxyRuntime) ServeEgressProxy(ctx context.Context, _ func(func(string) (string, bool)) http.Handler) error {
	<-ctx.Done()
	return nil
}

func (p *proxyRuntime) EgressProxyURL(string) (string, bool) { return "http://172.20.0.1:3128", true }

func (p *proxyRuntime) DeleteSandbox(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, id)
	return nil
}

func newProxyControl(t *testing.T, withSecret bool) (*SandboxControlGRPC, *proxyRuntime, *egressproxy.Server) {
	t.Helper()
	rt := &proxyRuntime{files: map[string]string{}}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, "SYSTEM CAS\n")
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			rt.mu.Lock()
			rt.files[r.URL.Query().Get("path")] = string(b)
			rt.mu.Unlock()
		}
	}))
	t.Cleanup(daemon.Close)
	rt.daemonURL = daemon.URL
	ca, err := egressproxy.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cfg := egressproxy.Config{CA: ca}
	dir := t.TempDir()
	if withSecret {
		_ = os.MkdirAll(filepath.Join(dir, "t1", "factory"), 0o700)
		_ = os.WriteFile(filepath.Join(dir, "t1", "factory", "git-token"), []byte("tok"), 0o600)
	}
	store, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg.Secrets = store
	srv := egressproxy.New(cfg)
	ctl := &SandboxControlGRPC{
		Runtime: rt, Cfg: &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec: metrics.NewRunnerRecorder(false), Cordon: cordon.New(), Egress: srv,
	}
	return ctl, rt, srv
}

const proxyOpts = `{"egress":"proxy","tenant":"t1","egress_proxy":{"allow":[{"name":"git","hosts":["git.example.com"]}],` +
	`"credentials":[{"host":"git.example.com","secret_ref":"factory/git-token"}]}}`

func TestProxyCreateInstallsPolicyAndCA(t *testing.T) {
	ctl, rt, srv := newProxyControl(t, true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	const id = "11111111-1111-4111-8111-111111111111"
	if _, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tenant, ok := srv.Tenant(id); !ok || tenant != "t1" {
		t.Fatalf("policy tenant = %q %v", tenant, ok)
	}
	if !rt.opts.BlockEgress() {
		t.Fatal("proxy sandbox created with direct egress")
	}
	ca := string(srv.CACertPEM())
	if rt.files[guestCAPath] != ca || !strings.HasPrefix(rt.files[guestBundlePath], "SYSTEM CAS\n") || !strings.HasSuffix(rt.files[guestBundlePath], ca) {
		t.Fatalf("guest files = %v", rt.files)
	}
	env := EgressExecEnv(srv, rt)(id)
	if env["HTTPS_PROXY"] != "http://172.20.0.1:3128" || env["SSL_CERT_FILE"] != guestBundlePath || env["NODE_EXTRA_CA_CERTS"] != guestCAPath {
		t.Fatalf("exec env = %v", env)
	}
	if EgressExecEnv(srv, rt)("22222222-2222-4222-8222-222222222222") != nil {
		t.Fatal("env for a sandbox that is not behind the proxy")
	}

	// Replace the spec; block model egress; delete forgets the policy.
	if _, err := ctl.UpdateEgressProxy(ctx, &pb.UpdateEgressProxyRequest{SandboxId: id, EgressProxyJson: `{"allow":[]}`}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := ctl.UpdateEgressProxy(ctx, &pb.UpdateEgressProxyRequest{SandboxId: id, EgressProxyJson: `{"allow":[],"x":1}`}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid update: %v", err)
	}
	if _, err := ctl.SetModelBlocked(ctx, &pb.SetModelBlockedRequest{SandboxId: id, Blocked: true}); err != nil {
		t.Fatalf("block: %v", err)
	}
	if _, err := ctl.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	if srv.Has(id) {
		t.Fatal("policy survived delete")
	}
	if _, err := ctl.SetModelBlocked(ctx, &pb.SetModelBlockedRequest{SandboxId: id, Blocked: true}); status.Code(err) != codes.NotFound {
		t.Fatalf("block after delete: %v", err)
	}
}

// Refused before anything exists: a secret the tenant lacks, model hosts with
// no sink, and a runtime that cannot host the proxy.
func TestProxyCreateRefusals(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	const id = "11111111-1111-4111-8111-111111111111"

	ctl, rt, srv := newProxyControl(t, false)
	_, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts})
	if status.Code(err) != codes.FailedPrecondition || rt.opts != nil || srv.Has(id) {
		t.Fatalf("missing secret: %v, created %v", err, rt.opts != nil)
	}

	ctl, rt, _ = newProxyControl(t, true)
	withModel := strings.Replace(proxyOpts, `}]}}`, `}],"model_hosts":[{"host":"git.example.com2","provider":"anthropic"}]}}`, 1)
	withModel = strings.Replace(withModel, `"hosts":["git.example.com"]`, `"hosts":["git.example.com","git.example.com2"]`, 1)
	if _, err := ctl.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: withModel}); status.Code(err) != codes.Unimplemented || rt.opts != nil {
		t.Fatalf("model hosts without a sink: %v", err)
	}

	plain := &SandboxControlGRPC{Runtime: &createOptsRuntime{}, Cfg: ctl.Cfg, Rec: ctl.Rec, Cordon: cordon.New(), Egress: ctl.Egress}
	if _, err := plain.CreateSandbox(ctx, &pb.CreateSandboxRequest{SandboxId: id, CreateOptionsJson: proxyOpts}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("runtime without proxy support: %v", err)
	}
	var _ runnerruntime.EgressProxyHost = (*proxyRuntime)(nil)
}
