package volumestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore/storepb"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// fakeHelper serves VolumeStore from memory. It follows the proto, so it
// passes the conformance suite, unless a test switches on a misbehaviour.
type fakeHelper struct {
	storepb.UnimplementedVolumeStoreServer
	socket string

	mu       sync.Mutex
	objects  map[string][]byte
	requests int
	commits  int
	received int // most data bytes one Put received
	readReqs []*storepb.ReadURLRequest
	readURL  string

	lax             bool          // ignores max_bytes and sha256, to show what the API enforces itself
	lieDigest       bool          // answers with a digest other than the stored one
	failAfterCommit bool          // stores the object, then fails the call
	earlyOK         int           // stores what arrived and answers OK after this many bytes
	earlyAnswered   chan struct{} // closed once that early answer went out
	shortGet        bool          // announces more bytes than it sends
	status          codes.Code    // fails every call with this code when set
	hang            chan struct{} // holds every call until closed
}

func newFakeHelper(t *testing.T) (*fakeHelper, *Remote) {
	t.Helper()
	// Unix socket paths are short; t.TempDir is too long on macOS.
	dir, err := os.MkdirTemp("", "vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	helper := &fakeHelper{
		socket:        filepath.Join(dir, "s"),
		objects:       map[string][]byte{},
		readURL:       "https://storage.test/object?sig=x",
		earlyAnswered: make(chan struct{}),
	}
	listener, err := net.Listen("unix", helper.socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(remoteMaxMessageSize), grpc.MaxSendMsgSize(remoteMaxMessageSize))
	storepb.RegisterVolumeStoreServer(server, helper)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	rs, err := NewRemote(helper.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	return helper, rs
}

// with runs inspect under the helper's lock, which its handlers hold while
// they read or change the same fields.
func (f *fakeHelper) with(inspect func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inspect()
}

// begin counts a call and applies the misbehaviours that hit every call.
func (f *fakeHelper) begin(ctx context.Context) error {
	f.mu.Lock()
	f.requests++
	code, hang := f.status, f.hang
	f.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return status.Error(codes.Unavailable, "hung")
	}
	if code != codes.OK {
		return status.Error(code, "helper says no")
	}
	return nil
}

func keyName(key *storepb.ObjectKey) (string, error) {
	kinds := map[storepb.Kind]volume.Kind{storepb.Kind_KIND_EXT4: volume.KindExt4, storepb.Kind_KIND_TAR: volume.KindTar}
	parsed := Key{Scope: key.GetScope(), ID: key.GetId(), Kind: kinds[key.GetKind()]}
	if err := parsed.Validate(); err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid key")
	}
	return parsed.objectName(), nil
}

func (f *fakeHelper) Put(stream storepb.VolumeStore_PutServer) error {
	if err := f.begin(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "the first message must carry the header")
	}
	name, err := keyName(header.GetKey())
	if err != nil {
		return err
	}
	var lax bool
	var earlyOK int
	f.with(func() { lax, earlyOK = f.lax, f.earlyOK })
	if !lax && (header.GetMaxBytes() <= 0 || (header.GetSha256() != "" && !volume.ValidDigest(header.GetSha256()))) {
		return status.Error(codes.InvalidArgument, "invalid header")
	}
	var data []byte
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err // ended without a half-close: nothing is stored
		}
		piece, ok := message.GetPart().(*storepb.PutRequest_Data)
		if !ok {
			return status.Error(codes.InvalidArgument, "only the first message may carry the header")
		}
		data = append(data, piece.Data...)
		f.with(func() { f.received = max(f.received, len(data)) })
		if !lax && int64(len(data)) > header.GetMaxBytes() {
			return status.Error(codes.OutOfRange, "more than max_bytes")
		}
		if earlyOK > 0 && len(data) >= earlyOK {
			f.with(func() { f.objects[name] = data })
			err := stream.SendAndClose(&storepb.Object{Size: int64(len(data)), Sha256: digest(data)})
			close(f.earlyAnswered)
			return err
		}
	}
	if !lax && header.GetSha256() != "" && header.GetSha256() != digest(data) {
		return status.Error(codes.DataLoss, "not the given sha256")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[name]; ok {
		return status.Error(codes.AlreadyExists, "the object exists")
	}
	f.objects[name] = data
	f.commits++
	if f.failAfterCommit {
		return status.Error(codes.Unavailable, "lost the answer")
	}
	sum := digest(data)
	if f.lieDigest {
		sum = digest([]byte("something else"))
	}
	return stream.SendAndClose(&storepb.Object{Size: int64(len(data)), Sha256: sum})
}

func (f *fakeHelper) Stat(ctx context.Context, request *storepb.StatRequest) (*storepb.Object, error) {
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	name, err := keyName(request.GetKey())
	if err != nil {
		return nil, err
	}
	var data []byte
	var ok bool
	f.with(func() { data, ok = f.objects[name] })
	if !ok {
		return nil, status.Error(codes.NotFound, "no such object")
	}
	return &storepb.Object{Size: int64(len(data)), Sha256: digest(data)}, nil
}

func (f *fakeHelper) Get(request *storepb.GetRequest, stream storepb.VolumeStore_GetServer) error {
	if err := f.begin(stream.Context()); err != nil {
		return err
	}
	name, err := keyName(request.GetKey())
	if err != nil {
		return err
	}
	var data []byte
	var ok, short bool
	f.with(func() {
		data, ok = f.objects[name]
		short = f.shortGet
	})
	if !ok {
		return status.Error(codes.NotFound, "no such object")
	}
	header := &storepb.Object{Size: int64(len(data)), Sha256: digest(data)}
	if short {
		data = data[:len(data)/2]
	}
	if err := stream.Send(&storepb.GetResponse{Part: &storepb.GetResponse_Object{Object: header}}); err != nil {
		return err
	}
	for len(data) > 0 {
		piece := data[:min(len(data), putPieceSize)]
		data = data[len(piece):]
		if err := stream.Send(&storepb.GetResponse{Part: &storepb.GetResponse_Data{Data: piece}}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeHelper) Delete(ctx context.Context, request *storepb.DeleteRequest) (*storepb.DeleteResponse, error) {
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	name, err := keyName(request.GetKey())
	if err != nil {
		return nil, err
	}
	f.with(func() { delete(f.objects, name) })
	return &storepb.DeleteResponse{}, nil
}

func (f *fakeHelper) ReadURL(ctx context.Context, request *storepb.ReadURLRequest) (*storepb.ReadURLResponse, error) {
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	if _, err := keyName(request.GetKey()); err != nil {
		return nil, err
	}
	if request.GetTtlSeconds() < 1 || request.GetTtlSeconds() > 3600 {
		return nil, status.Error(codes.InvalidArgument, "ttl_seconds must be 1 to 3600")
	}
	var readURL string
	f.with(func() {
		f.readReqs = append(f.readReqs, request)
		readURL = f.readURL
	})
	return &storepb.ReadURLResponse{Url: readURL}, nil
}

func TestRemotePutStatOpenDelete(t *testing.T) {
	// The helper is reached over its socket only, never through a proxy.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	helper, rs := newFakeHelper(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("volume!\n"), putPieceSize/8*2+5) // three pieces
	want := Object{Size: int64(len(data)), SHA256: digest(data)}

	obj, err := rs.Put(ctx, tenantKey, bytes.NewReader(data), PutOptions{MaxBytes: int64(len(data)), ExpectedSHA256: want.SHA256})
	if err != nil || obj != want {
		t.Fatalf("Put = %+v, %v; want %+v", obj, err, want)
	}
	var stored []byte
	helper.with(func() { stored = helper.objects["tenants/"+testTenant+"/deps.tar"] })
	if !bytes.Equal(stored, data) {
		t.Fatalf("helper stored %d bytes under the object name, want %d", len(stored), len(data))
	}
	if _, err := rs.Put(ctx, tenantKey, bytes.NewReader(data), PutOptions{MaxBytes: int64(len(data))}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}
	if got, err := rs.Stat(ctx, tenantKey); err != nil || got != want {
		t.Fatalf("Stat = %+v, %v; want %+v", got, err, want)
	}
	reader, got, err := rs.Open(ctx, tenantKey)
	if err != nil || got != want {
		t.Fatalf("Open = %+v, %v; want %+v", got, err, want)
	}
	content, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(content, data) {
		t.Fatalf("Open content: %d bytes, %v", len(content), err)
	}
	if err := rs.Delete(ctx, tenantKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := rs.Delete(ctx, tenantKey); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
	if _, err := rs.Stat(ctx, tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if _, _, err := rs.Open(ctx, tenantKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
	}
}

// The helper is lax and stores any stream that ends with a half-close, so
// nothing may be stored unless the API itself cut the upload off.
func TestRemotePutCutsOffBeforeTheHelperCommits(t *testing.T) {
	data := []byte("0123456789")
	cases := []struct {
		name   string
		opts   PutOptions
		reader io.Reader
		want   error
	}{
		{"too large", PutOptions{MaxBytes: 9}, bytes.NewReader(data), ErrTooLarge},
		{"digest mismatch", PutOptions{MaxBytes: 10, ExpectedSHA256: digest([]byte("other"))}, bytes.NewReader(data), ErrDigestMismatch},
		{"reader error", PutOptions{MaxBytes: 100}, io.MultiReader(bytes.NewReader(data), iotestErrReader{}), errRead},
		{"too large with an error in the same read", PutOptions{MaxBytes: 9}, readerFunc(func(buf []byte) (int, error) { return copy(buf, data), errRead }), ErrTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			helper, rs := newFakeHelper(t)
			helper.with(func() { helper.lax = true })
			if _, err := rs.Put(context.Background(), sharedKey, testCase.reader, testCase.opts); !errors.Is(err, testCase.want) {
				t.Fatalf("Put = %v, want %v", err, testCase.want)
			}
			// The helper may still be reading the cancelled stream.
			time.Sleep(50 * time.Millisecond)
			helper.with(func() {
				if helper.commits != 0 {
					t.Errorf("helper committed %d objects", helper.commits)
				}
				if int64(helper.received) > testCase.opts.MaxBytes {
					t.Errorf("helper received %d bytes, past MaxBytes %d", helper.received, testCase.opts.MaxBytes)
				}
			})
		})
	}
}

func TestRemoteMapsHelperStatuses(t *testing.T) {
	ctx := context.Background()
	put := func(rs *Remote) error {
		_, err := rs.Put(ctx, sharedKey, strings.NewReader("x"), PutOptions{MaxBytes: 1})
		return err
	}
	stat := func(rs *Remote) error { _, err := rs.Stat(ctx, sharedKey); return err }
	cases := []struct {
		code codes.Code
		call func(*Remote) error
		want error
	}{
		{codes.AlreadyExists, put, ErrExists},
		{codes.OutOfRange, put, ErrTooLarge},
		{codes.DataLoss, put, ErrDigestMismatch},
		{codes.InvalidArgument, put, ErrInvalidKey},
		{codes.NotFound, stat, ErrNotFound},
		{codes.InvalidArgument, stat, ErrInvalidKey},
	}
	for _, testCase := range cases {
		helper, rs := newFakeHelper(t)
		helper.with(func() { helper.status = testCase.code })
		if err := testCase.call(rs); !errors.Is(err, testCase.want) {
			t.Errorf("%s: %v, want %v", testCase.code, err, testCase.want)
		}
	}

	helper, rs := newFakeHelper(t)
	helper.with(func() { helper.status = codes.Unavailable })
	for name, call := range map[string]func(*Remote) error{
		"Put":    put,
		"Stat":   stat,
		"Delete": func(rs *Remote) error { return rs.Delete(ctx, sharedKey) },
		"Open": func(rs *Remote) error {
			_, _, err := rs.Open(ctx, sharedKey)
			return err
		},
		"ReadURL": func(rs *Remote) error {
			_, err := rs.ReadURL(ctx, sharedKey, time.Minute)
			return err
		},
	} {
		err := call(rs)
		if err == nil || !strings.Contains(err.Error(), "Unavailable") || !strings.Contains(err.Error(), "helper says no") {
			t.Errorf("%s with a failing helper = %v", name, err)
		}
	}
}

func TestRemoteRefusesAnAnswerThatDoesNotMatchTheUpload(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.with(func() { helper.lieDigest = true })
	if _, err := rs.Put(context.Background(), sharedKey, strings.NewReader("data"), PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put accepted a digest other than the one sent")
	}
	var left int
	helper.with(func() { left = len(helper.objects) })
	if left != 0 {
		t.Fatalf("%d mismatched objects left behind", left)
	}
}

// The helper stores what has arrived and answers OK while the upload is
// still open.
func TestRemotePutDiscardsAnObjectStoredBeforeTheUploadEnded(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.with(func() { helper.earlyOK = 3 })
	resumed := false
	rest := readerFunc(func(buf []byte) (int, error) {
		if resumed {
			return 0, io.EOF
		}
		<-helper.earlyAnswered
		resumed = true
		return copy(buf, "def"), nil
	})
	reader := io.MultiReader(strings.NewReader("abc"), rest)
	if _, err := rs.Put(context.Background(), sharedKey, reader, PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put accepted an object stored before the upload ended")
	}
	var left int
	helper.with(func() { left = len(helper.objects) })
	if left != 0 {
		t.Fatalf("%d early objects left behind", left)
	}
}

// After the whole upload went out, a failed answer leaves the outcome open:
// the object decides.
func TestRemotePutSettlesAnOpenOutcome(t *testing.T) {
	helper, rs := newFakeHelper(t)
	helper.with(func() { helper.failAfterCommit = true })
	data := []byte("data")
	obj, err := rs.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10})
	if want := (Object{Size: 4, SHA256: digest(data)}); err != nil || obj != want {
		t.Fatalf("Put = %+v, %v; want the stored object %+v", obj, err, want)
	}

	helper, rs = newFakeHelper(t)
	helper.with(func() { helper.status = codes.Internal })
	if _, err := rs.Put(context.Background(), sharedKey, bytes.NewReader(data), PutOptions{MaxBytes: 10}); err == nil {
		t.Fatal("Put succeeded although nothing was stored")
	}
}

func TestRemoteOpenHoldsTheHelperToTheAnnouncedSize(t *testing.T) {
	helper, rs := newFakeHelper(t)
	if _, err := rs.Put(context.Background(), sharedKey, strings.NewReader("0123456789"), PutOptions{MaxBytes: 10}); err != nil {
		t.Fatal(err)
	}
	helper.with(func() { helper.shortGet = true })
	reader, _, err := rs.Open(context.Background(), sharedKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading a short object = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestRemoteReadURL(t *testing.T) {
	helper, rs := newFakeHelper(t)
	ctx := context.Background()
	var wantURL string
	helper.with(func() { wantURL = helper.readURL })
	got, err := rs.ReadURL(ctx, tenantKey, 24*time.Hour)
	if err != nil || got != wantURL {
		t.Fatalf("ReadURL = %q, %v; want %q", got, err, wantURL)
	}
	if _, err := rs.ReadURL(ctx, tenantKey, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var readReqs []*storepb.ReadURLRequest
	helper.with(func() { readReqs = slices.Clone(helper.readReqs) })
	if len(readReqs) != 2 {
		t.Fatalf("helper got %d read URL requests, want 2", len(readReqs))
	}
	for index, wantTTL := range []int64{3600, 2} {
		if readReqs[index].GetTtlSeconds() != wantTTL || readReqs[index].GetKey().GetScope() != testTenant || readReqs[index].GetKey().GetId() != "deps" {
			t.Errorf("request %d = %v, want ttl %d for the tenant's deps", index, readReqs[index], wantTTL)
		}
	}
	if _, err := rs.ReadURL(ctx, tenantKey, 0); err == nil {
		t.Error("ReadURL with a zero ttl succeeded")
	}
	for _, bad := range []string{"", "file:///etc/passwd", "/relative", "https://", "https://:443/x", "javascript:alert(1)",
		"http://storage.test/x", "https://user:pw@storage.test/x", "https://storage.test/x#y"} {
		helper.with(func() { helper.readURL = bad })
		if _, err := rs.ReadURL(ctx, tenantKey, time.Minute); err == nil {
			t.Errorf("helper URL %q accepted", bad)
		}
	}
}

func TestRemoteRejectsInvalidKeysWithoutCallingTheHelper(t *testing.T) {
	helper, rs := newFakeHelper(t)
	ctx := context.Background()
	bad := Key{Scope: volume.SharedScope, ID: "../../x", Kind: volume.KindTar}
	if _, err := rs.Put(ctx, bad, strings.NewReader("x"), PutOptions{MaxBytes: 1}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Put = %v", err)
	}
	if _, err := rs.Stat(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Stat = %v", err)
	}
	if _, _, err := rs.Open(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Open = %v", err)
	}
	if err := rs.Delete(ctx, bad); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Delete = %v", err)
	}
	if _, err := rs.ReadURL(ctx, bad, time.Minute); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ReadURL = %v", err)
	}
	var requests int
	helper.with(func() { requests = helper.requests })
	if requests != 0 {
		t.Fatalf("helper got %d requests", requests)
	}
}

func TestRemoteWithoutHelper(t *testing.T) {
	rs, err := NewRemote(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if _, err := rs.Stat(context.Background(), sharedKey); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat without a helper = %v, want a connection error", err)
	}
}

func TestRemoteReportsTheCallersCancellation(t *testing.T) {
	_, rs := newFakeHelper(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rs.Stat(ctx, sharedKey); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestRemoteGivesUpOnAStuckHelper(t *testing.T) {
	previous := remoteIdleTimeout
	remoteIdleTimeout = remoteIdleTimeoutForTest
	helper, rs := newFakeHelper(t)
	remoteIdleTimeout = previous
	hang := make(chan struct{})
	helper.with(func() { helper.hang = hang })
	t.Cleanup(func() { close(hang) })

	for name, call := range map[string]func() error{
		"Stat": func() error { _, err := rs.Stat(context.Background(), sharedKey); return err },
		"Put": func() error {
			_, err := rs.Put(context.Background(), sharedKey, strings.NewReader("data"), PutOptions{MaxBytes: 10})
			return err
		},
	} {
		start := time.Now()
		if err := call(); err == nil {
			t.Errorf("%s on a stuck helper succeeded", name)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s took %s on a stuck helper", name, elapsed)
		}
	}
}

// The watchdog ends stalls only: an upload that keeps moving, and a reader
// that pauses between reads, outlive it.
func TestRemoteIdleTimeoutSparesSlowTransfers(t *testing.T) {
	const idle = 500 * time.Millisecond
	previous := remoteIdleTimeout
	remoteIdleTimeout = idle
	helper, rs := newFakeHelper(t)
	remoteIdleTimeout = previous
	ctx := context.Background()

	slow := readerFunc(func(buf []byte) (int, error) {
		time.Sleep(idle / 4)
		buf[0] = 'x'
		return 1, nil
	})
	if _, err := rs.Put(ctx, sharedKey, io.LimitReader(slow, 8), PutOptions{MaxBytes: 8}); err != nil {
		t.Fatalf("slow upload: %v", err)
	}

	// Larger than both flow-control windows, so the stream is still open
	// during the pause.
	big := bytes.Repeat([]byte("volume"), 3<<20)
	helper.with(func() { helper.objects["tenants/"+testTenant+"/deps.tar"] = big })
	reader, _, err := rs.Open(ctx, tenantKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	head := make([]byte, 1024)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * idle)
	rest, err := io.ReadAll(reader)
	if err != nil || len(head)+len(rest) != len(big) {
		t.Fatalf("download after a pause: %d bytes, %v", len(head)+len(rest), err)
	}
}

const remoteIdleTimeoutForTest = 200 * time.Millisecond

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(buf []byte) (int, error) { return f(buf) }
