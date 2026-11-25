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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// tarEntry is one entry of a test tar.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	// mode is 0o644 when 0.
	mode int64
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
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: cmp.Or(entry.mode, 0o644), Size: int64(len(entry.body)), Linkname: entry.linkname}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if entry.typeflag == tar.TypeXGlobalHeader {
			// Metadata for the whole archive, its body as a pax comment.
			header = &tar.Header{Typeflag: entry.typeflag, PAXRecords: map[string]string{"comment": entry.body}}
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
		tarEntry{name: "bin/run.sh", typeflag: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		link("bin/readme", "../docs/guide/readme.md"),
	)
}

// trackingRecords records which rows were started and purged, and can fail
// some calls.
type trackingRecords struct {
	store.SharedVolumes
	mutex   sync.Mutex
	started []string
	purged  []string
	// finishLosesReply commits a finish but reports an error, as a lost
	// database reply would.
	finishLosesReply bool
	// renewHangs makes lease renewals wait until their context ends.
	renewHangs bool
	renewals   int
}

func (records *trackingRecords) RenewSharedVolumePublish(ctx context.Context, id string, lease time.Duration) (bool, error) {
	records.mutex.Lock()
	records.renewals++
	hangs := records.renewHangs
	records.mutex.Unlock()
	if hangs {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return records.SharedVolumes.RenewSharedVolumePublish(ctx, id, lease)
}

func (records *trackingRecords) renewalCount() int {
	records.mutex.Lock()
	defer records.mutex.Unlock()
	return records.renewals
}

func (records *trackingRecords) StartSharedVolumePublish(ctx context.Context, volume *store.SharedVolume, lease time.Duration, maxLiveVolumes int) (*store.SharedVolume, error) {
	live, err := records.SharedVolumes.StartSharedVolumePublish(ctx, volume, lease, maxLiveVolumes)
	if err == nil {
		records.mutex.Lock()
		records.started = append(records.started, volume.ID)
		records.mutex.Unlock()
	}
	return live, err
}

func (records *trackingRecords) PurgeSharedVolume(ctx context.Context, id string) (bool, error) {
	purged, err := records.SharedVolumes.PurgeSharedVolume(ctx, id)
	if purged {
		records.mutex.Lock()
		records.purged = append(records.purged, id)
		records.mutex.Unlock()
	}
	return purged, err
}

func (records *trackingRecords) FinishSharedVolumePublish(ctx context.Context, volume *store.SharedVolume) (bool, error) {
	finished, err := records.SharedVolumes.FinishSharedVolumePublish(ctx, volume)
	if err == nil && records.finishLosesReply {
		return false, errors.New("connection reset")
	}
	return finished, err
}

// failingObjects fails Put or Delete on request.
type failingObjects struct {
	volumestore.Store
	failPut    bool
	failDelete bool
}

func (objects *failingObjects) Put(ctx context.Context, key volumestore.Key, content io.Reader, opts volumestore.PutOptions) (volumestore.Object, error) {
	if objects.failPut {
		return volumestore.Object{}, errors.New("disk on fire")
	}
	return objects.Store.Put(ctx, key, content, opts)
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

// The caps of testPolicy, small enough to reach in a test.
const (
	testMaxEntries           = 16
	testMaxContentBytes      = 16 << 10 // 16 KiB
	testMaxDecompressedBytes = 64 << 10 // 64 KiB
)

var testPolicy = volume.TarPolicy(testMaxEntries, testMaxContentBytes, testMaxDecompressedBytes)

// testMaxUploadBytes is the upload limit of the service in tests.
const testMaxUploadBytes = 1 << 20 // 1 MiB

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	storeDir := filepath.Join(t.TempDir(), "volumes")
	local, err := volumestore.NewLocal(storeDir, "https://api.test:8443", volumestore.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sqlite, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	ctx, shutdown := context.WithCancel(context.Background())
	t.Cleanup(shutdown)
	env := &testEnv{
		records:  &trackingRecords{SharedVolumes: sqlite},
		local:    local,
		objects:  &failingObjects{Store: local},
		storeDir: storeDir,
		metrics:  metrics.NewSharedVolumeRecorder(metrics.NewAPIRecorder( /* enabled= */ true)),
		shutdown: shutdown,
	}
	env.service = NewService(ctx, env.records, env.objects, env.metrics)
	env.service.limits = limits{maxTarBytes: 1 << 20, policy: testPolicy, maxLiveVolumes: 100}
	env.service.timing = timing{lease: time.Minute, renewEvery: time.Hour, giveUpAfter: time.Hour, cleanupTimeout: 5 * time.Second}
	return env
}

func (env *testEnv) publish(t *testing.T, data []byte, name string) (PublishResult, error) {
	t.Helper()
	return env.service.Publish(context.Background(), PublishRequest{
		SHA256: digestOf(data), Name: name, SizeBytes: int64(len(data)), Body: bytes.NewReader(data), Role: RoleVolumePublisher,
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
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// assertNothingLeft checks that every started row was purged and the store
// holds no file.
func (env *testEnv) assertNothingLeft(t *testing.T) {
	t.Helper()
	env.records.mutex.Lock()
	started, purged := slices.Clone(env.records.started), slices.Clone(env.records.purged)
	env.records.mutex.Unlock()
	slices.Sort(started)
	slices.Sort(purged)
	if !slices.Equal(started, purged) {
		t.Errorf("rows started %v, purged %v; want every one purged", started, purged)
	}
	if files := env.storedFiles(t); len(files) > 0 {
		t.Errorf("the store holds %v, want nothing", files)
	}
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
	if reader.failAt > 0 {
		end = min(end, reader.failAt)
	}
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
