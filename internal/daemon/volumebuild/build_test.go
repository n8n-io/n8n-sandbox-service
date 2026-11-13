package volumebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

type tarEntry struct {
	hdr  tar.Header
	body string
}

func tarFile(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body: body}
}

func tarExecutable(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o4755}, body: body}
}

func tarDir(name string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}

func tarSymlink(name, target string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

func tarHardLink(name, target string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target}}
}

func writeTar(t *testing.T, out io.Writer, entries ...tarEntry) {
	t.Helper()
	tw := tar.NewWriter(out)
	for _, entry := range entries {
		hdr := entry.hdr
		hdr.Size = int64(len(entry.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write tar header %q: %v", hdr.Name, err)
		}
		if _, err := io.WriteString(tw, entry.body); err != nil {
			t.Fatalf("write tar body %q: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writeTar(t, &buf, entries...)
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// paramsFor is what the API sends for a tar: its length and what the volume
// policy counts in it.
func paramsFor(t *testing.T, body []byte) Params {
	t.Helper()
	stats, err := safetar.Validate(bytes.NewReader(body), volume.TarPolicy(maxEntries, 0, 0))
	if err != nil {
		t.Fatalf("validate tar: %v", err)
	}
	return Params{TarBytes: int64(len(body)), Entries: int64(stats.Entries), ContentBytes: stats.Bytes, DecompressedBytes: stats.DecompressedBytes}
}

// fakeSys records the calls a Builder makes and fails the ones in fail. Its
// tar drive is tar followed by zeros, as the runner pads it.
type fakeSys struct {
	pid, euid int
	tar       []byte
	// tarReadErr, if set, is what the tar drive fails with after a sector.
	tarReadErr error
	fail       map[string]error
	calls      []string
	status     []byte
}

func newFakeSys(tar []byte) *fakeSys {
	return &fakeSys{pid: 1, tar: tar, fail: map[string]error{}}
}

func (s *fakeSys) call(name string) error {
	s.calls = append(s.calls, name)
	return s.fail[name]
}

func (s *fakeSys) Pid() int  { return s.pid }
func (s *fakeSys) Euid() int { return s.euid }

func (s *fakeSys) Tar(device string, length int64) (io.ReadCloser, error) {
	if err := s.call("tar " + device); err != nil {
		return nil, err
	}
	var drive io.Reader = bytes.NewReader(append(slices.Clone(s.tar), make([]byte, 2*sectorBytes)...))
	if s.tarReadErr != nil {
		drive = io.MultiReader(bytes.NewReader(s.tar[:sectorBytes]), iotest.ErrReader(s.tarReadErr))
	}
	return io.NopCloser(io.LimitReader(drive, length)), nil
}

func (s *fakeSys) Format(device string, inodes int64) error {
	return s.call(fmt.Sprintf("format %s %d", device, inodes))
}
func (s *fakeSys) Mount(device, path string) error { return s.call("mount " + device) }
func (s *fakeSys) UsedBytes(string) (int64, error) { return 8192, s.call("used") }
func (s *fakeSys) Flush(string) error              { return s.call("flush") }
func (s *fakeSys) Unmount(string) error            { return s.call("unmount") }
func (s *fakeSys) Check(device string) error       { return s.call("check " + device) }
func (s *fakeSys) Reboot() error                   { return s.call("reboot") }

func (s *fakeSys) WriteStatus(device string, record []byte) error {
	s.status = slices.Clone(record)
	return s.call("status " + device)
}

func newTestBuilder(t *testing.T, sys Sys) *Builder {
	t.Helper()
	builder := New(sys)
	builder.mountPoint = t.TempDir()
	return builder
}

func recordedStatus(t *testing.T, sys *fakeSys) Status {
	t.Helper()
	status, err := ParseStatus(sys.status)
	if err != nil {
		t.Fatalf("the status record does not parse: %v\n%q", err, bytes.TrimRight(sys.status, "\x00"))
	}
	return status
}

func TestRunBuildsRecordsAndReboots(t *testing.T) {
	body := buildTar(t,
		tarDir("docs/"),
		tarFile("docs/a.txt", "alpha"),
		tarExecutable("bin/run.sh", "#!/bin/sh\n"),
		tarSymlink("bin/link", "../docs/a.txt"),
	)
	sys := newFakeSys(body)
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}

	wantCalls := []string{
		"tar /dev/vdb",
		fmt.Sprintf("format /dev/vdc %d", Inodes(4)),
		"mount /dev/vdc",
		"used",
		"flush",
		"unmount",
		"check /dev/vdc",
		"status /dev/vdd",
		"reboot",
	}
	if !slices.Equal(sys.calls, wantCalls) {
		t.Errorf("calls:\n got %q\nwant %q", sys.calls, wantCalls)
	}
	status := recordedStatus(t, sys)
	if status.Result != ResultOK || status.Reason != "" || status.Files != 2 || status.Bytes != 15 || status.UsedBytes != 8192 {
		t.Errorf("status = %+v", status)
	}

	if got, err := os.ReadFile(filepath.Join(builder.mountPoint, "docs/a.txt")); err != nil || string(got) != "alpha" {
		t.Errorf("docs/a.txt = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(builder.mountPoint, "bin/run.sh")); err != nil || info.Mode() != 0o755 {
		t.Errorf("bin/run.sh: %v, %v; want mode 0755 without setuid", info, err)
	}
	if target, err := os.Readlink(filepath.Join(builder.mountPoint, "bin/link")); err != nil || target != "../docs/a.txt" {
		t.Errorf("bin/link -> %q, %v", target, err)
	}
}

func TestRunReadsAGzipTarUpToItsLength(t *testing.T) {
	body := gzipped(t, buildTar(t, tarFile("a.txt", "alpha")))
	sys := newFakeSys(body)
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if status := recordedStatus(t, sys); status.Result != ResultOK {
		t.Fatalf("status = %+v: the zeros after the tar must not be read", status)
	}
}

func TestRunRefusesAnythingButInit(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "alpha"))
	for name, identity := range map[string]struct{ pid, euid int }{
		"the sandbox user":     {pid: 4242, euid: 1000},
		"root, not init":       {pid: 4242, euid: 0},
		"init without root":    {pid: 1, euid: 1000},
		"a container's daemon": {pid: 1, euid: 1000},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newFakeSys(body)
			sys.pid, sys.euid = identity.pid, identity.euid
			builder := newTestBuilder(t, sys)

			if err := builder.Run(paramsFor(t, body), nil); !errors.Is(err, ErrNotInit) {
				t.Fatalf("Run() = %v, want %v", err, ErrNotInit)
			}
			if len(sys.calls) != 0 || sys.status != nil {
				t.Errorf("touched the system: %q", sys.calls)
			}
			if entries, err := os.ReadDir(builder.mountPoint); err != nil || len(entries) != 0 {
				t.Errorf("unpacked something: %v, %v", entries, err)
			}
		})
	}
}

func TestRunMapsEachFailureToItsReason(t *testing.T) {
	good := buildTar(t, tarDir("d/"), tarFile("d/a.txt", "alpha"))
	goodParams := paramsFor(t, good)
	fewer := func(change func(*Params)) Params {
		params := goodParams
		change(&params)
		return params
	}
	conflict := buildTar(t, tarFile("a", "x"), tarFile("a/b", "y"))
	hardLink := buildTar(t, tarFile("a", "x"), tarHardLink("b", "a"))
	// Small on the tar drive, a megabyte more than counted once inflated.
	inflating := gzipped(t, append(slices.Clone(good), make([]byte, 1<<20)...))
	longPath := buildTar(t, tarDir(strings.Repeat("d/", 2048)))

	tests := []struct {
		name      string
		tar       []byte
		params    Params
		paramsErr error
		failCall  string
		want      string
		// wantCalls are the calls from the mount on.
		wantCalls []string
	}{
		{name: "invalid parameters", tar: good, params: goodParams, paramsErr: errors.New("bad"), want: ReasonInternal,
			wantCalls: []string{"status /dev/vdd", "reboot"}},
		{name: "parameters out of range", tar: good, params: Params{TarBytes: 1}, want: ReasonInternal,
			wantCalls: []string{"status /dev/vdd", "reboot"}},
		{name: "tar drive", tar: good, params: goodParams, failCall: "tar /dev/vdb", want: ReasonInternal,
			wantCalls: []string{"status /dev/vdd", "reboot"}},
		{name: "format", tar: good, params: goodParams, failCall: fmt.Sprintf("format /dev/vdc %d", Inodes(2)), want: ReasonFormat,
			wantCalls: []string{"status /dev/vdd", "reboot"}},
		{name: "mount", tar: good, params: goodParams, failCall: "mount /dev/vdc", want: ReasonMount,
			wantCalls: []string{"mount /dev/vdc", "status /dev/vdd", "reboot"}},
		{name: "not a tar", tar: bytes.Repeat([]byte("junk"), 400), params: Params{TarBytes: 1600, Entries: 1, DecompressedBytes: 1600}, want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "truncated tar", tar: good, params: fewer(func(params *Params) { params.TarBytes = 700 }), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "hard link", tar: hardLink, params: Params{TarBytes: int64(len(hardLink)), Entries: 2, ContentBytes: 1, DecompressedBytes: int64(len(hardLink))}, want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "a path longer than a system call takes", tar: longPath, params: Params{TarBytes: int64(len(longPath)), Entries: 1, DecompressedBytes: int64(len(longPath))}, want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "entries that do not fit together", tar: conflict, params: paramsFor(t, conflict), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "more entries than counted", tar: good, params: fewer(func(params *Params) { params.Entries-- }), want: ReasonTooLarge,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "more content than counted", tar: good, params: fewer(func(params *Params) { params.ContentBytes-- }), want: ReasonTooLarge,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "fewer entries than counted", tar: good, params: fewer(func(params *Params) { params.Entries++ }), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "less content than counted", tar: good, params: fewer(func(params *Params) { params.ContentBytes++ }), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "content where none was counted", tar: good, params: fewer(func(params *Params) { params.ContentBytes = 0 }), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "more decompressed than counted", tar: good, params: fewer(func(params *Params) { params.DecompressedBytes-- }), want: ReasonTooLarge,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "less decompressed than counted", tar: good, params: fewer(func(params *Params) { params.DecompressedBytes++ }), want: ReasonTar,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "a gzip tar that inflates past its count", tar: inflating, params: fewer(func(params *Params) { params.TarBytes = int64(len(inflating)) }), want: ReasonTooLarge,
			wantCalls: []string{"mount /dev/vdc", "unmount", "status /dev/vdd", "reboot"}},
		{name: "flush", tar: good, params: goodParams, failCall: "flush", want: ReasonInternal,
			wantCalls: []string{"mount /dev/vdc", "used", "flush", "unmount", "status /dev/vdd", "reboot"}},
		{name: "unmount", tar: good, params: goodParams, failCall: "unmount", want: ReasonMount,
			wantCalls: []string{"mount /dev/vdc", "used", "flush", "unmount", "status /dev/vdd", "reboot"}},
		{name: "check", tar: good, params: goodParams, failCall: "check /dev/vdc", want: ReasonFsck,
			wantCalls: []string{"mount /dev/vdc", "used", "flush", "unmount", "check /dev/vdc", "status /dev/vdd", "reboot"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := newFakeSys(tt.tar)
			if tt.failCall != "" {
				sys.fail[tt.failCall] = errors.New("injected failure")
			}
			builder := newTestBuilder(t, sys)

			if err := builder.Run(tt.params, tt.paramsErr); err != nil {
				t.Fatalf("Run() failed: %v", err)
			}
			status := recordedStatus(t, sys)
			if status.Result != ResultFailed || status.Reason != tt.want {
				t.Errorf("status = %+v, want reason %q", status, tt.want)
			}
			fromMount := sys.calls
			if index := slices.Index(sys.calls, "mount /dev/vdc"); index >= 0 {
				fromMount = sys.calls[index:]
			} else {
				fromMount = sys.calls[len(sys.calls)-2:]
			}
			if !slices.Equal(fromMount, tt.wantCalls) {
				t.Errorf("calls from the mount on:\n got %q\nwant %q", fromMount, tt.wantCalls)
			}
		})
	}
}

func TestRunReportsAFullDriveAsNoSpace(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "alpha"))
	sys := newFakeSys(body)
	sys.fail["flush"] = fmt.Errorf("syncfs: %w", syscall.ENOSPC)
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if status := recordedStatus(t, sys); status.Reason != ReasonNoSpace {
		t.Errorf("status = %+v, want reason %q", status, ReasonNoSpace)
	}
}

// safetar reports a failed read as an invalid archive, which would blame the
// caller's tar for the runner's drive.
func TestRunBlamesAFailedTarDriveOnTheRunner(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", strings.Repeat("a", 4*sectorBytes)))
	sys := newFakeSys(body)
	sys.tarReadErr = syscall.EIO
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if status := recordedStatus(t, sys); status.Result != ResultFailed || status.Reason != ReasonInternal {
		t.Errorf("status = %+v, want reason %q", status, ReasonInternal)
	}
	if !slices.Contains(sys.calls, "unmount") {
		t.Errorf("the image stayed mounted: %q", sys.calls)
	}
}

func TestExtractReason(t *testing.T) {
	entry := func(err error) error {
		return &safetar.EntryError{Name: "name", Err: &fs.PathError{Op: "openat", Path: "name", Err: err}}
	}
	for name, tt := range map[string]struct {
		err  error
		want string
	}{
		"out of blocks or inodes":   {entry(syscall.ENOSPC), ReasonNoSpace},
		"a file below a file":       {entry(syscall.ENOTDIR), ReasonTar},
		"a file over a directory":   {entry(syscall.EISDIR), ReasonTar},
		"a link over an entry":      {entry(syscall.EEXIST), ReasonTar},
		"a name too long for ext4":  {entry(syscall.ENAMETOOLONG), ReasonTar},
		"a failed write":            {entry(syscall.EIO), ReasonInternal},
		"an invalid archive":        {fmt.Errorf("%w: bad header", safetar.ErrInvalid), ReasonTar},
		"a cap":                     {fmt.Errorf("%w: more than 3 entries", safetar.ErrTooLarge), ReasonTooLarge},
		"the mount point went away": {&fs.PathError{Op: "open", Path: "/mnt", Err: syscall.ENOENT}, ReasonInternal},
	} {
		if got := extractReason(tt.err); got != tt.want {
			t.Errorf("%s: reason %q, want %q", name, got, tt.want)
		}
	}
}

func TestRunRebootsWhenTheStatusCannotBeWritten(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "alpha"))
	sys := newFakeSys(body)
	sys.fail["status /dev/vdd"] = errors.New("injected failure")
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if last := sys.calls[len(sys.calls)-1]; last != "reboot" {
		t.Errorf("last call %q, want reboot", last)
	}
}

func TestRunReturnsAFailedReboot(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "alpha"))
	sys := newFakeSys(body)
	sys.fail["reboot"] = syscall.EPERM
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("Run() = %v, want the reboot's error", err)
	}
}

func TestStatusRecordHoldsNoTextFromTheTar(t *testing.T) {
	const marker = "TENANTTEXT"
	tars := map[string][]byte{
		"ok": buildTar(t,
			tarFile(marker+"-file", marker+" content"),
			tarSymlink(marker+"-link", "../"+marker+"-target"),
		),
		"refused entry": buildTar(t, tarFile(marker+"-file", "x"), tarHardLink(marker+"-hard", marker+"-file")),
		"conflict":      buildTar(t, tarFile(marker, "x"), tarFile(marker+"/"+marker, "y")),
	}
	for name, body := range tars {
		t.Run(name, func(t *testing.T) {
			sys := newFakeSys(body)
			params := Params{TarBytes: int64(len(body)), Entries: 2, ContentBytes: 2, DecompressedBytes: int64(len(body))}
			if name == "ok" {
				params = paramsFor(t, body)
			}
			if err := newTestBuilder(t, sys).Run(params, nil); err != nil {
				t.Fatalf("Run() failed: %v", err)
			}

			if len(sys.status) != StatusBytes {
				t.Fatalf("record has %d bytes, want %d", len(sys.status), StatusBytes)
			}
			raw, padding, _ := bytes.Cut(sys.status, []byte{0})
			if !json.Valid(raw) {
				t.Errorf("record is not JSON: %q", raw)
			}
			if strings.Trim(string(padding), "\x00") != "" {
				t.Errorf("record is not padded with zeros")
			}
			if bytes.Contains(sys.status, []byte(marker)) {
				t.Errorf("record holds text from the tar: %q", raw)
			}
			if status := recordedStatus(t, sys); (status.Result == ResultOK) != (name == "ok") {
				t.Errorf("status = %+v", status)
			}
		})
	}
}

func TestBuildTurnsAPanicIntoAStatus(t *testing.T) {
	body := buildTar(t, tarFile("a.txt", "alpha"))
	sys := &panickingSys{fakeSys: newFakeSys(body)}
	builder := newTestBuilder(t, sys)

	if err := builder.Run(paramsFor(t, body), nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if status := recordedStatus(t, sys.fakeSys); status.Result != ResultFailed || status.Reason != ReasonInternal {
		t.Errorf("status = %+v", status)
	}
}

type panickingSys struct{ *fakeSys }

func (*panickingSys) Format(string, int64) error { panic("boom") }
