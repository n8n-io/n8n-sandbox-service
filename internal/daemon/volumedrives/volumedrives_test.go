package volumedrives

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func volumesBody(t *testing.T, volumes ...Volume) string {
	t.Helper()
	body, err := json.Marshal(map[string][]Volume{"volumes": volumes})
	if err != nil {
		t.Fatalf("marshal volumes: %v", err)
	}
	return string(body)
}

func TestParseRequest(t *testing.T) {
	longest := "/mnt/" + strings.Repeat("a", maxPathBytes-len("/mnt/"))
	nine := make([]Volume, 9)
	for i := range nine {
		nine[i] = Volume{Drive: i, Sectors: 1, Path: "/mnt/v" + string(rune('0'+i))}
	}
	eight := nine[:8]
	vol := func(drive int, sectors uint64, path string) Volume {
		return Volume{Drive: drive, Sectors: sectors, Path: path}
	}

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"empty list", `{"volumes":[]}`, ""},
		{"no volumes field", `{}`, ""},
		{"one volume", `{"volumes":[{"drive":0,"sectors":265416,"path":"/mnt/knowledge"}]}`, ""},
		{"eight volumes", volumesBody(t, eight...), ""},
		{"below home", volumesBody(t, vol(7, 1, "/home/user/data/set")), ""},
		{"sibling with a common prefix", volumesBody(t, vol(0, 1, "/mnt/a"), vol(1, 1, "/mnt/ab")), ""},
		{"longest path", volumesBody(t, vol(0, 1, longest)), ""},

		{"nine volumes", volumesBody(t, nine...), "at most 8 volumes"},
		{"negative drive", volumesBody(t, vol(-1, 1, "/mnt/a")), "drive must be 0 to 7"},
		{"drive past the last", volumesBody(t, vol(8, 1, "/mnt/a")), "drive must be 0 to 7"},
		{"drive listed twice", volumesBody(t, vol(3, 1, "/mnt/a"), vol(3, 1, "/mnt/b")), "drive 3 is listed twice"},
		{"no sectors", volumesBody(t, vol(0, 0, "/mnt/a")), "sectors out of range"},
		{"sectors overflow bytes", volumesBody(t, vol(0, 1<<55, "/mnt/a")), "sectors out of range"},
		{"negative sectors", `{"volumes":[{"drive":0,"sectors":-1,"path":"/mnt/a"}]}`, "cannot unmarshal"},
		{"drive as string", `{"volumes":[{"drive":"0","sectors":1,"path":"/mnt/a"}]}`, "cannot unmarshal"},
		{"relative path", volumesBody(t, vol(0, 1, "mnt/a")), "not absolute and clean"},
		{"empty path", volumesBody(t, vol(0, 1, "")), "not absolute and clean"},
		{"dot-dot", volumesBody(t, vol(0, 1, "/mnt/a/../b")), "not absolute and clean"},
		{"escape through dot-dot", volumesBody(t, vol(0, 1, "/mnt/../etc")), "not absolute and clean"},
		{"double slash", volumesBody(t, vol(0, 1, "/mnt//a")), "not absolute and clean"},
		{"trailing slash", volumesBody(t, vol(0, 1, "/mnt/a/")), "not absolute and clean"},
		{"mnt itself", volumesBody(t, vol(0, 1, "/mnt")), "not below"},
		{"home itself", volumesBody(t, vol(0, 1, "/home/user")), "not below"},
		{"another home", volumesBody(t, vol(0, 1, "/home/username/a")), "not below"},
		{"mnt as a prefix", volumesBody(t, vol(0, 1, "/mntx/a")), "not below"},
		{"system path", volumesBody(t, vol(0, 1, "/etc/cron.d")), "not below"},
		{"path too long", volumesBody(t, vol(0, 1, longest+"a")), "longer than 255 bytes"},
		{"NUL in path", volumesBody(t, vol(0, 1, "/mnt/a\x00b")), "control character"},
		{"newline in path", volumesBody(t, vol(0, 1, "/mnt/a\nb")), "control character"},
		{"same path twice", volumesBody(t, vol(0, 1, "/mnt/a"), vol(1, 1, "/mnt/a")), "overlaps"},
		{"path inside an earlier one", volumesBody(t, vol(0, 1, "/mnt/a"), vol(1, 1, "/mnt/a/b")), "overlaps"},
		{"path around an earlier one", volumesBody(t, vol(0, 1, "/mnt/a/b"), vol(1, 1, "/mnt/a")), "overlaps"},
		{"unknown field", `{"volumes":[],"extra":true}`, "unknown field"},
		{"unknown volume field", `{"volumes":[{"drive":0,"sectors":1,"path":"/mnt/a","rw":true}]}`, "unknown field"},
		{"trailing value", `{"volumes":[]}{}`, "single JSON value"},
		{"not JSON", `volumes`, "invalid character"},
		{"empty body", ``, "EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseRequest() error = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseRequest() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDevicePath(t *testing.T) {
	for drive, want := range map[int]string{0: "/dev/vdb", 1: "/dev/vdc", 7: "/dev/vdi"} {
		if got := devicePath(drive); got != want {
			t.Errorf("devicePath(%d) = %s, want %s", drive, got, want)
		}
	}
}

// fakeSys records system calls. Size reads are only counted: how many there
// are depends on timing.
type fakeSys struct {
	mu         sync.Mutex
	calls      []string
	sizes      map[string]uint64 // what a device reports once it has its size
	readyAfter map[string]int    // size reads that report 0 before that
	sizeReads  map[string]int
	errs       map[string]error // keyed by the recorded call
}

func newFakeSys() *fakeSys {
	return &fakeSys{
		sizes:      map[string]uint64{},
		readyAfter: map[string]int{},
		sizeReads:  map[string]int{},
		errs:       map[string]error{},
	}
}

func (f *fakeSys) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.errs[call]
}

func (f *fakeSys) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeSys) DeviceSize(device string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizeReads[device]++
	if f.sizeReads[device] <= f.readyAfter[device] {
		return 0, nil
	}
	return f.sizes[device], nil
}

func (f *fakeSys) FlushBuffers(device string) error { return f.record("flush " + device) }
func (f *fakeSys) MakeMountPoint(path string) error { return f.record("mkdir " + path) }
func (f *fakeSys) Unmount(path string) error        { return f.record("unmount " + path) }
func (f *fakeSys) Mount(device, path string) error  { return f.record("mount " + device + " " + path) }

func testMounter(sys Sys) *Mounter {
	m := NewMounter(sys)
	m.sizeTimeout = 50 * time.Millisecond
	m.pollInterval = time.Millisecond
	return m
}

func requireCalls(t *testing.T, sys *fakeSys, want ...string) {
	t.Helper()
	if got := sys.recorded(); !slices.Equal(got, want) {
		t.Fatalf("system calls:\n got %q\nwant %q", got, want)
	}
}

func TestMountAllMountsEachVolumeInOrder(t *testing.T) {
	sys := newFakeSys()
	sys.sizes["/dev/vdb"] = 8 * 512
	sys.sizes["/dev/vdc"] = 16 * 512

	err := testMounter(sys).MountAll([]Volume{
		{Drive: 0, Sectors: 8, Path: "/mnt/a"},
		{Drive: 1, Sectors: 16, Path: "/home/user/b"},
	})
	if err != nil {
		t.Fatalf("MountAll() error = %v", err)
	}
	requireCalls(t, sys,
		"flush /dev/vdb", "mkdir /mnt/a", "mount /dev/vdb /mnt/a",
		"flush /dev/vdc", "mkdir /home/user/b", "mount /dev/vdc /home/user/b",
	)
}

func TestMountAllWaitsForTheNewSize(t *testing.T) {
	sys := newFakeSys()
	sys.sizes["/dev/vdb"] = 8 * 512
	sys.readyAfter["/dev/vdb"] = 3

	if err := testMounter(sys).MountAll([]Volume{{Drive: 0, Sectors: 8, Path: "/mnt/a"}}); err != nil {
		t.Fatalf("MountAll() error = %v", err)
	}
	if got := sys.sizeReads["/dev/vdb"]; got != 4 {
		t.Fatalf("size reads = %d, want 4: three before the size arrived and the one that saw it", got)
	}
}

func TestMountAllRefusesADeviceWithoutItsSize(t *testing.T) {
	for name, size := range map[string]uint64{"never resized": 0, "another size": 9 * 512} {
		t.Run(name, func(t *testing.T) {
			sys := newFakeSys()
			sys.sizes["/dev/vdb"] = 8 * 512
			sys.sizes["/dev/vdc"] = size

			err := testMounter(sys).MountAll([]Volume{
				{Drive: 0, Sectors: 8, Path: "/mnt/a"},
				{Drive: 1, Sectors: 8, Path: "/mnt/b"},
			})
			if err == nil || !strings.Contains(err.Error(), "want 4096") {
				t.Fatalf("MountAll() error = %v, want the size mismatch", err)
			}
			if sys.sizeReads["/dev/vdc"] < 2 {
				t.Fatalf("size reads = %d, want the mount to have waited", sys.sizeReads["/dev/vdc"])
			}
			requireCalls(t, sys, "flush /dev/vdb", "mkdir /mnt/a", "mount /dev/vdb /mnt/a", "unmount /mnt/a")
		})
	}
}

func TestMountAllRollsBackWhatItMounted(t *testing.T) {
	tests := []struct {
		name, failing, reason string
	}{
		{"size never arrives", "", "drive"},
		{"flush", "flush /dev/vdd", "drive"},
		{"mkdir", "mkdir /mnt/c", "mount_point"},
		{"mount", "mount /dev/vdd /mnt/c", "mount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := newFakeSys()
			for _, dev := range []string{"/dev/vdb", "/dev/vdc", "/dev/vdd", "/dev/vde"} {
				sys.sizes[dev] = 512
			}
			if tt.failing != "" {
				sys.errs[tt.failing] = errors.New("boom")
			} else {
				delete(sys.sizes, "/dev/vdd")
			}

			err := testMounter(sys).MountAll([]Volume{
				{Drive: 0, Sectors: 1, Path: "/mnt/a"},
				{Drive: 1, Sectors: 1, Path: "/mnt/b"},
				{Drive: 2, Sectors: 1, Path: "/mnt/c"},
				{Drive: 3, Sectors: 1, Path: "/mnt/d"},
			})
			if err == nil || !strings.Contains(err.Error(), "volume drive 2 at /mnt/c") {
				t.Fatalf("MountAll() error = %v, want the third volume's failure", err)
			}
			if got := Reason(err); got != tt.reason {
				t.Fatalf("Reason() = %q, want %q", got, tt.reason)
			}
			calls := sys.recorded()
			if slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "/dev/vde") || strings.Contains(c, "/mnt/d") }) {
				t.Fatalf("calls %q went on past the failed volume", calls)
			}
			if got, want := calls[len(calls)-2:], []string{"unmount /mnt/b", "unmount /mnt/a"}; !slices.Equal(got, want) {
				t.Fatalf("calls end with %q, want %q: the mounted volumes undone, latest first", got, want)
			}
			if slices.Contains(calls, "unmount /mnt/c") {
				t.Fatalf("calls %q unmount the volume that failed", calls)
			}
		})
	}
}

func TestMountAllReportsAFailedRollback(t *testing.T) {
	sys := newFakeSys()
	sys.sizes["/dev/vdb"] = 512
	sys.sizes["/dev/vdc"] = 512
	sys.errs["mount /dev/vdc /mnt/b"] = errors.New("bad superblock")
	sys.errs["unmount /mnt/a"] = errors.New("device busy")

	err := testMounter(sys).MountAll([]Volume{
		{Drive: 0, Sectors: 1, Path: "/mnt/a"},
		{Drive: 1, Sectors: 1, Path: "/mnt/b"},
	})
	if err == nil || !strings.Contains(err.Error(), "bad superblock") || !strings.Contains(err.Error(), "roll back /mnt/a: device busy") {
		t.Fatalf("MountAll() error = %v, want the mount failure and the failed rollback", err)
	}
}

func TestMountAllWithNoVolumesTouchesNothing(t *testing.T) {
	sys := newFakeSys()
	if err := testMounter(sys).MountAll(nil); err != nil {
		t.Fatalf("MountAll() error = %v", err)
	}
	requireCalls(t, sys)
}

func TestReasonOfAnotherError(t *testing.T) {
	if got := Reason(errors.New("boom")); got != "" {
		t.Fatalf("Reason() = %q, want none", got)
	}
}

// tempRoot is a temporary directory with no symlink on its path (macOS puts
// t.TempDir under one).
func tempRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMkdirNoFollowCreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(tempRoot(t), "mnt", "a", "b")
	for range 2 {
		if err := mkdirNoFollow(dir); err != nil {
			t.Fatalf("mkdirNoFollow() error = %v", err)
		}
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("mount point not created: %v", err)
	}
}

func TestMkdirNoFollowRefusesSymlinksAndFiles(t *testing.T) {
	root := tempRoot(t)
	outside := tempRoot(t)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"link", "link/b", "file", "file/b"} {
		if err := mkdirNoFollow(filepath.Join(root, dir)); err == nil {
			t.Fatalf("mkdirNoFollow(%s) succeeded, want it refused", dir)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("created %v through the symlink", entries)
	}
}
