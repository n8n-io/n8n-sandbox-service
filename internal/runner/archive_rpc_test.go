package runner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/archive"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/activity"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// archivingRuntime exports and imports a fixed disk.
type archivingRuntime struct {
	createOptsRuntime
	disk     []byte
	imported []byte
}

func (a *archivingRuntime) ExportSandbox(_ context.Context, _ string, w io.Writer) error {
	_, err := w.Write(a.disk)
	return err
}

func (a *archivingRuntime) ImportSandbox(_ context.Context, id string, opts *runnerruntime.CreateOptions, r io.Reader) (*runnerruntime.SandboxInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	a.imported, a.opts = b, opts
	return &runnerruntime.SandboxInfo{ID: id, IP: "10.0.0.9"}, nil
}

type objectStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (o *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		o.objs[r.URL.Path] = b
	case http.MethodGet:
		if r.URL.Path == "/cut" {
			// The connection drops halfway through the object.
			b := o.objs["/a1"]
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			_, _ = w.Write(b[:len(b)/2])
			return
		}
		b, ok := o.objs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}
}

func TestArchiveAndRestoreRoundTrip(t *testing.T) {
	store := &objectStore{objs: map[string][]byte{}}
	srv := httptest.NewServer(store)
	defer srv.Close()
	rt := &archivingRuntime{disk: bytes.Repeat([]byte("disk "), 50_000)}
	tr := activity.New()
	ctl := &SandboxControlGRPC{Runtime: rt, Cfg: &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec: metrics.NewRunnerRecorder(false), Cordon: cordon.New(), Activity: tr}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	const id = "11111111-1111-4111-8111-111111111111"
	key, _ := archive.NewKey()

	tr.ExecStarted(id, "e1", time.Hour)
	if _, err := ctl.ArchiveSandbox(ctx, &pb.ArchiveSandboxRequest{SandboxId: id, UploadUrl: srv.URL + "/a1", Key: key}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("archive while active: %v", err)
	}
	tr.ExecEnded(id, "e1")

	resp, err := ctl.ArchiveSandbox(ctx, &pb.ArchiveSandboxRequest{SandboxId: id, UploadUrl: srv.URL + "/a1", Key: key})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	obj := store.objs["/a1"]
	if resp.GetSize() != int64(len(obj)) || bytes.Contains(obj, []byte("disk disk")) {
		t.Fatalf("size %d vs %d, or the object is not sealed", resp.GetSize(), len(obj))
	}

	out, err := ctl.RestoreSandbox(ctx, &pb.RestoreSandboxRequest{SandboxId: id, CreateOptionsJson: `{"egress":"none"}`, DownloadUrl: srv.URL + "/a1", Key: key})
	if err != nil || !bytes.Equal(rt.imported, rt.disk) || out.GetAppliedCreateOptionsJson() != `{"egress":"none"}` || !rt.opts.BlockEgress() {
		t.Fatalf("restore: %v, same disk %v", err, bytes.Equal(rt.imported, rt.disk))
	}

	// Tampered, truncated, or opened with another key: DataLoss.
	store.objs["/bad"] = append([]byte(nil), obj...)
	store.objs["/bad"][len(obj)/2] ^= 1
	store.objs["/short"] = obj[:len(obj)/2]
	other, _ := archive.NewKey()
	for _, tc := range []struct {
		path string
		key  []byte
	}{{"/bad", key}, {"/short", key}, {"/a1", other}} {
		_, err := ctl.RestoreSandbox(ctx, &pb.RestoreSandboxRequest{SandboxId: id, DownloadUrl: srv.URL + tc.path, Key: tc.key})
		if status.Code(err) != codes.DataLoss {
			t.Errorf("%s: %v, want DataLoss", tc.path, err)
		}
	}
	// A missing object or a dropped download says nothing about the archive,
	// and the error carries no signed URL.
	for _, path := range []string{"/missing", "/cut"} {
		_, err := ctl.RestoreSandbox(ctx, &pb.RestoreSandboxRequest{SandboxId: id, DownloadUrl: srv.URL + path + "?sig=secret", Key: key})
		if err == nil || status.Code(err) == codes.DataLoss || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v, want a failure that is not DataLoss", path, err)
		}
	}

	// A runtime that cannot archive says so; a non-http URL is refused.
	plain := &SandboxControlGRPC{Runtime: &createOptsRuntime{}, Cfg: ctl.Cfg, Rec: ctl.Rec, Cordon: cordon.New()}
	if _, err := plain.ArchiveSandbox(ctx, &pb.ArchiveSandboxRequest{SandboxId: id, UploadUrl: srv.URL + "/x", Key: key}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("plain archive: %v", err)
	}
	if _, err := ctl.ArchiveSandbox(ctx, &pb.ArchiveSandboxRequest{SandboxId: id, UploadUrl: "file:///etc/passwd", Key: key}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("file URL: %v", err)
	}
}
