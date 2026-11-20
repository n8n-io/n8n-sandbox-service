package volumes

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore/storepb"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// tarEntry is one entry of a test tar.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

func dir(name string) tarEntry        { return tarEntry{name: name, typeflag: tar.TypeDir} }
func file(name, body string) tarEntry { return tarEntry{name: name, typeflag: tar.TypeReg, body: body} }
func link(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeSymlink, linkname: target}
}

func makeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: 0o644, Size: int64(len(entry.body)), Linkname: entry.linkname}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validTar has a folder the tar leaves out (docs), an executable and a link.
func validTar(t *testing.T) []byte {
	return makeTar(t,
		file("docs/guide/readme.md", "read me\n"),
		dir("bin"),
		file("bin/run.sh", "#!/bin/sh\n"),
		link("bin/readme", "../docs/guide/readme.md"),
	)
}

// trackingRecords records which rows were reserved and removed, and can fail
// some calls.
type trackingRecords struct {
	store.SharedVolumes
	mutex    sync.Mutex
	reserved []string
	removed  []string
	// finishLosesReply commits a finish but reports an error, as a lost
	// database reply would.
	finishLosesReply bool
	// renewHangs makes lease renewals wait until their context ends.
	renewHangs bool
	renewals   int
}

func (records *trackingRecords) RenewSharedVolumeLease(ctx context.Context, id string, leaseUntil int64) (bool, error) {
	records.mutex.Lock()
	records.renewals++
	hangs := records.renewHangs
	records.mutex.Unlock()
	if hangs {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return records.SharedVolumes.RenewSharedVolumeLease(ctx, id, leaseUntil)
}

func (records *trackingRecords) renewalCount() int {
	records.mutex.Lock()
	defer records.mutex.Unlock()
	return records.renewals
}

func (records *trackingRecords) ReserveSharedVolume(ctx context.Context, volume *store.SharedVolume, maxLive int) (*store.SharedVolume, error) {
	live, err := records.SharedVolumes.ReserveSharedVolume(ctx, volume, maxLive)
	if err == nil {
		records.mutex.Lock()
		records.reserved = append(records.reserved, volume.ID)
		records.mutex.Unlock()
	}
	return live, err
}

func (records *trackingRecords) RemoveSharedVolume(ctx context.Context, id string) (bool, error) {
	removed, err := records.SharedVolumes.RemoveSharedVolume(ctx, id)
	if removed {
		records.mutex.Lock()
		records.removed = append(records.removed, id)
		records.mutex.Unlock()
	}
	return removed, err
}

func (records *trackingRecords) FinishSharedVolume(ctx context.Context, volume *store.SharedVolume) (bool, error) {
	finished, err := records.SharedVolumes.FinishSharedVolume(ctx, volume)
	if err == nil && records.finishLosesReply {
		return false, errors.New("connection reset")
	}
	return finished, err
}

// failingObjects fails Put or Delete on request, and counts the Puts started
// per kind.
type failingObjects struct {
	volumestore.Store
	failPut    bool
	failDelete bool
	// failImagePut fails an image's Put after its first read.
	failImagePut bool
	// failTarRead fails reading an opened object after its first bytes.
	failTarRead bool
	mutex       sync.Mutex
	puts        map[volume.Kind]int
}

func (objects *failingObjects) Put(ctx context.Context, key volumestore.Key, content io.Reader, opts volumestore.PutOptions) (volumestore.Object, error) {
	objects.mutex.Lock()
	if objects.puts == nil {
		objects.puts = map[volume.Kind]int{}
	}
	objects.puts[key.Kind]++
	objects.mutex.Unlock()
	if objects.failPut {
		return volumestore.Object{}, errors.New("disk on fire")
	}
	if objects.failImagePut && key.Kind == volume.KindExt4 {
		if _, err := content.Read(make([]byte, 1)); err != nil {
			return volumestore.Object{}, err
		}
		return volumestore.Object{}, errors.New("disk on fire")
	}
	return objects.Store.Put(ctx, key, content, opts)
}

func (objects *failingObjects) Open(ctx context.Context, key volumestore.Key) (io.ReadCloser, volumestore.Object, error) {
	content, object, err := objects.Store.Open(ctx, key)
	if err != nil || !objects.failTarRead {
		return content, object, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(content, 100), iotest.ErrReader(errors.New("disk on fire"))), content}, object, nil
}

func (objects *failingObjects) putsOf(kind volume.Kind) int {
	objects.mutex.Lock()
	defer objects.mutex.Unlock()
	return objects.puts[kind]
}

func (objects *failingObjects) Delete(ctx context.Context, key volumestore.Key) error {
	if objects.failDelete {
		return errors.New("disk on fire")
	}
	return objects.Store.Delete(ctx, key)
}

type testEnv struct {
	service  *Service
	records  *trackingRecords
	local    *volumestore.Local
	objects  *failingObjects
	storeDir string
	metrics  *metrics.SharedVolumeRecorder
	shutdown context.CancelFunc
}

// testPolicy keeps the caps small enough to reach in a test.
var testPolicy = volume.TarPolicy(16, 1<<14, 1<<16)

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dataDir := t.TempDir()
	local, err := volumestore.NewLocal(dataDir, "https://api.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	return newTestEnvWith(t, local, filepath.Join(dataDir, "volumes"))
}

func newTestEnvWith(t *testing.T, objects volumestore.Store, storeDir string) *testEnv {
	t.Helper()
	sqlite, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	ctx, shutdown := context.WithCancel(context.Background())
	t.Cleanup(shutdown)
	env := &testEnv{
		records:  &trackingRecords{SharedVolumes: sqlite},
		objects:  &failingObjects{Store: objects},
		storeDir: storeDir,
		metrics:  metrics.NewSharedVolumeRecorder(metrics.NewAPIRecorder(true)),
		shutdown: shutdown,
	}
	if local, ok := objects.(*volumestore.Local); ok {
		env.local = local
	}
	env.service = NewService(ctx, env.records, env.objects, env.metrics)
	env.service.limits = limits{
		maxTarBytes:   1 << 20, // 1 MiB
		policy:        testPolicy,
		maxVolumes:    100,
		maxImageBytes: MaxImageBytes, maxRunnerBytes: volume.MaxSandboxVolumeBytes,
	}
	env.service.timing = timing{
		lease: time.Minute, renewEvery: time.Hour, giveUp: time.Hour, cleanup: 5 * time.Second,
		builderWait: 200 * time.Millisecond, retryFirst: 10 * time.Millisecond, retryMax: 50 * time.Millisecond,
		buildCall:  10 * time.Second,
		sweepEvery: time.Hour, failedGrace: 10 * time.Minute,
	}
	return env
}

// withBuilds has env's service build images with build, on runners.
func (env *testEnv) withBuilds(build BuildFunc, runners ...registry.Runner) *testEnv {
	env.service.BuildImages(Builds{Runners: fixedRunners(runners), Build: build, HeartbeatGrace: time.Minute})
	return env
}

// fixedRunners lists the same runners every time.
type fixedRunners []registry.Runner

func (runners fixedRunners) List() ([]registry.Runner, error) {
	return slices.Clone(runners), nil
}

// builder returns a runner that builds, heard from just now.
func builder(id string, used int32) registry.Runner {
	return registry.Runner{
		ID: id, HTTPBaseURL: "https://" + id, ControlGRPCAddr: id + ":9091",
		Healthy: true, CapacityTotal: 4, CapacityUsed: used, LastSeen: time.Now(),
	}
}

// fakeBuilds answers builds like runners would. A runner with an answer in
// refuse answers with it before it reads the tar; any other reads the tar,
// fails with fail if set after half the image, or sends image.
type fakeBuilds struct {
	refuse map[string]error
	fail   error
	image  []byte
	// claim, if set, is the sha256 the runner says its image has.
	claim string
	// beforeImage, if set, runs before the first image byte is sent.
	beforeImage func()

	mutex   sync.Mutex
	calls   []string
	headers []*pb.BuildVolumeHeader
	tars    [][]byte
}

func newFakeBuilds(image []byte) *fakeBuilds {
	return &fakeBuilds{refuse: map[string]error{}, image: image}
}

func (builds *fakeBuilds) build(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (runnerctl.VolumeImage, error) {
	builds.mutex.Lock()
	builds.calls = append(builds.calls, runner.ID)
	builds.headers = append(builds.headers, header)
	refusal := builds.refuse[runner.ID]
	builds.mutex.Unlock()
	if refusal != nil {
		return runnerctl.VolumeImage{}, refusal
	}
	data, err := io.ReadAll(tar)
	if err != nil {
		return runnerctl.VolumeImage{}, err
	}
	builds.mutex.Lock()
	builds.tars = append(builds.tars, data)
	builds.mutex.Unlock()
	if builds.beforeImage != nil {
		builds.beforeImage()
	}
	half := len(builds.image) / 2
	if _, err := image.Write(builds.image[:half]); err != nil {
		return runnerctl.VolumeImage{}, err
	}
	if builds.fail != nil {
		return runnerctl.VolumeImage{}, builds.fail
	}
	if _, err := image.Write(builds.image[half:]); err != nil {
		return runnerctl.VolumeImage{}, err
	}
	return runnerctl.VolumeImage{Bytes: int64(len(builds.image)), SHA256: cmp.Or(builds.claim, digestOf(builds.image))}, ctx.Err()
}

func (builds *fakeBuilds) callsMade() []string {
	builds.mutex.Lock()
	defer builds.mutex.Unlock()
	return slices.Clone(builds.calls)
}

func (env *testEnv) publish(t *testing.T, data []byte, name string) (Published, error) {
	t.Helper()
	return env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(data), Name: name, Size: int64(len(data)), Body: bytes.NewReader(data), Role: "publisher",
	})
}

// storedFiles lists every file under the store's directory, temp files too.
func (env *testEnv) storedFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(env.storeDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, strings.TrimPrefix(path, env.storeDir))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return files
}

// assertNothingLeft checks that every reserved row was removed and the store
// holds no file.
func (env *testEnv) assertNothingLeft(t *testing.T) {
	t.Helper()
	env.records.mutex.Lock()
	reserved, removed := slices.Clone(env.records.reserved), slices.Clone(env.records.removed)
	env.records.mutex.Unlock()
	slices.Sort(reserved)
	slices.Sort(removed)
	if !slices.Equal(reserved, removed) {
		t.Errorf("rows reserved %v, removed %v; want every one removed", reserved, removed)
	}
	if files := env.storedFiles(t); len(files) > 0 {
		t.Errorf("the store holds %v, want nothing", files)
	}
}

// memoryHelper is a store helper that keeps objects in memory and holds the
// client to the proto: a valid key and header first, then only data. It
// stores only a half-closed upload of at most max_bytes with the given sha256.
type memoryHelper struct {
	storepb.UnimplementedVolumeStoreServer
	mutex   sync.Mutex
	objects map[string][]byte
}

// helperObjectName names the object of key, and refuses an invalid key.
func helperObjectName(key *storepb.ObjectKey) (string, error) {
	kinds := map[storepb.Kind]volume.Kind{storepb.Kind_KIND_EXT4: volume.KindExt4, storepb.Kind_KIND_TAR: volume.KindTar}
	parsed := volumestore.Key{Scope: key.GetScope(), ID: key.GetId(), Kind: kinds[key.GetKind()]}
	if err := parsed.Validate(); err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid key")
	}
	return key.GetScope() + "/" + key.GetId() + "." + key.GetKind().String(), nil
}

func (helper *memoryHelper) Put(stream storepb.VolumeStore_PutServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "no header")
	}
	name, err := helperObjectName(header.GetKey())
	if err != nil {
		return err
	}
	if header.GetMaxBytes() <= 0 || (header.GetSha256() != "" && !volume.ValidDigest(header.GetSha256())) {
		return status.Error(codes.InvalidArgument, "invalid header")
	}
	var data []byte
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		piece, ok := message.GetPart().(*storepb.PutRequest_Data)
		if !ok {
			return status.Error(codes.InvalidArgument, "only the first message may carry the header")
		}
		data = append(data, piece.Data...)
		if int64(len(data)) > header.GetMaxBytes() {
			return status.Error(codes.OutOfRange, "too large")
		}
	}
	if header.GetSha256() != "" && header.GetSha256() != digestOf(data) {
		return status.Error(codes.DataLoss, "digest")
	}
	helper.mutex.Lock()
	defer helper.mutex.Unlock()
	if _, exists := helper.objects[name]; exists {
		return status.Error(codes.AlreadyExists, "exists")
	}
	helper.objects[name] = data
	return stream.SendAndClose(&storepb.Object{Size: int64(len(data)), Sha256: digestOf(data)})
}

func (helper *memoryHelper) Stat(_ context.Context, request *storepb.StatRequest) (*storepb.Object, error) {
	name, err := helperObjectName(request.GetKey())
	if err != nil {
		return nil, err
	}
	helper.mutex.Lock()
	defer helper.mutex.Unlock()
	data, ok := helper.objects[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &storepb.Object{Size: int64(len(data)), Sha256: digestOf(data)}, nil
}

func (helper *memoryHelper) Get(request *storepb.GetRequest, stream storepb.VolumeStore_GetServer) error {
	name, err := helperObjectName(request.GetKey())
	if err != nil {
		return err
	}
	helper.mutex.Lock()
	data, ok := helper.objects[name]
	helper.mutex.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "not found")
	}
	object := &storepb.Object{Size: int64(len(data)), Sha256: digestOf(data)}
	if err := stream.Send(&storepb.GetResponse{Part: &storepb.GetResponse_Object{Object: object}}); err != nil {
		return err
	}
	for len(data) > 0 {
		piece := data[:min(len(data), 64<<10)] // 64 KiB a message
		data = data[len(piece):]
		if err := stream.Send(&storepb.GetResponse{Part: &storepb.GetResponse_Data{Data: piece}}); err != nil {
			return err
		}
	}
	return nil
}

func (helper *memoryHelper) Delete(_ context.Context, request *storepb.DeleteRequest) (*storepb.DeleteResponse, error) {
	name, err := helperObjectName(request.GetKey())
	if err != nil {
		return nil, err
	}
	helper.mutex.Lock()
	defer helper.mutex.Unlock()
	delete(helper.objects, name)
	return &storepb.DeleteResponse{}, nil
}

func (helper *memoryHelper) count() int {
	helper.mutex.Lock()
	defer helper.mutex.Unlock()
	return len(helper.objects)
}

func newMemoryHelper(t *testing.T) (*memoryHelper, *volumestore.Remote) {
	t.Helper()
	// Unix socket paths are short; t.TempDir is too long on macOS.
	socketDir, err := os.MkdirTemp("", "vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	helper := &memoryHelper{objects: map[string][]byte{}}
	server := grpc.NewServer()
	storepb.RegisterVolumeStoreServer(server, helper)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	remote, err := volumestore.NewRemote(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	return helper, remote
}

// chunkedReader hands out data in pieces, waiting for each piece's turn on
// next, then fails with failAt bytes read if failAt is set. A wait ends like
// a read past its deadline once stop is closed.
type chunkedReader struct {
	data   []byte
	piece  int
	next   chan struct{}
	stop   chan struct{}
	failAt int
	read   int
}

func (reader *chunkedReader) Read(buf []byte) (int, error) {
	if reader.failAt > 0 && reader.read >= reader.failAt {
		return 0, errors.New("connection reset by peer")
	}
	if reader.read >= len(reader.data) {
		return 0, io.EOF
	}
	if reader.next != nil {
		select {
		case <-reader.next:
		case <-reader.stop:
			return 0, os.ErrDeadlineExceeded
		}
	}
	end := min(reader.read+reader.piece, len(reader.data), reader.read+len(buf))
	copied := copy(buf, reader.data[reader.read:end])
	reader.read += copied
	return copied, nil
}

// unreadable fails the test if anything reads it.
type unreadable struct{ t *testing.T }

func (body unreadable) Read([]byte) (int, error) {
	body.t.Error("the body was read")
	return 0, io.EOF
}
