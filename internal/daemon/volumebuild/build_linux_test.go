//go:build linux

package volumebuild

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// These tests build real images. They need root and loop devices, which a
// privileged container has:
//
//	docker run --rm --privileged -v "$PWD":/src -w /src golang:1.27-bookworm \
//	  go test -count=1 -v -run TestReal ./internal/daemon/volumebuild/

// initSys is the real system, as the init of a builder microVM sees it.
type initSys struct {
	Sys
	// inodes, if set, replaces the inode count of the format.
	inodes   int64
	rebooted bool
}

func (s *initSys) Pid() int { return 1 }

func (s *initSys) Reboot() error {
	s.rebooted = true
	return nil
}

func (s *initSys) Format(device string, inodes int64) error {
	if s.inodes > 0 {
		inodes = s.inodes
	}
	return s.Sys.Format(device, inodes)
}

// loopDevice attaches a new file of that size to a free loop device.
func loopDevice(t *testing.T, size int64) string {
	t.Helper()
	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("needs loop devices: %v", err)
	}
	defer control.Close()

	backing, err := os.Create(filepath.Join(t.TempDir(), "output.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer backing.Close()
	if err := backing.Truncate(size); err != nil {
		t.Fatal(err)
	}

	for range 16 {
		number, err := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			t.Skipf("needs loop devices: %v", err)
		}
		device := fmt.Sprintf("/dev/loop%d", number)
		// A container's /dev does not follow the host's.
		if _, err := os.Stat(device); errors.Is(err, fs.ErrNotExist) {
			if err := unix.Mknod(device, unix.S_IFBLK|0o600, int(unix.Mkdev(7, uint32(number)))); err != nil {
				t.Fatalf("create %s: %v", device, err)
			}
		}
		loop, err := os.OpenFile(device, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open %s: %v", device, err)
		}
		err = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_SET_FD, int(backing.Fd()))
		if errors.Is(err, unix.EBUSY) {
			loop.Close()
			continue
		}
		if err != nil {
			loop.Close()
			t.Fatalf("attach %s: %v", device, err)
		}
		t.Cleanup(func() {
			if err := unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0); err != nil {
				t.Errorf("detach %s: %v", device, err)
			}
			loop.Close()
		})
		return device
	}
	t.Fatal("no free loop device")
	return ""
}

// tarDrive writes the tar to a file as the runner does: padded to whole
// sectors, here with a sector more.
func tarDrive(t *testing.T, write func(io.Writer)) (path string, params Params) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	write(file)
	length, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(DriveBytes(length) + sectorBytes); err != nil {
		t.Fatal(err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stats, err := safetar.Validate(io.LimitReader(file, length), volume.TarPolicy(maxEntries, 0, 0))
	if err != nil {
		// A plain tar the API would refuse: the caller sets the counts.
		return path, Params{TarBytes: length, DecompressedBytes: length}
	}
	return path, Params{TarBytes: length, Entries: int64(stats.Entries), ContentBytes: stats.Bytes, DecompressedBytes: stats.DecompressedBytes}
}

func entriesDrive(t *testing.T, entries ...tarEntry) (string, Params) {
	t.Helper()
	return tarDrive(t, func(out io.Writer) { writeTar(t, out, entries...) })
}

type realBuild struct {
	sys    *initSys
	output string
	status Status
}

// runRealBuild builds the tar into a loop device of outputBytes, or of the
// size the runner would give it if outputBytes is 0.
func runRealBuild(t *testing.T, tarPath string, params Params, outputBytes int64, inodes int64) realBuild {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if outputBytes == 0 {
		outputBytes = ImageBytes(params)
	}
	build := realBuild{sys: &initSys{Sys: System(), inodes: inodes}, output: loopDevice(t, outputBytes)}

	statusPath := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(statusPath, make([]byte, StatusBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := New(build.sys)
	builder.tar, builder.output, builder.status, builder.mountPoint = tarPath, build.output, statusPath, t.TempDir()

	if err := builder.Run(params, nil); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if !build.sys.rebooted {
		t.Error("the build did not end with a reboot")
	}
	record, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if build.status, err = ParseStatus(record); err != nil {
		t.Fatalf("the status record does not parse: %v", err)
	}
	if entries, err := os.ReadDir(builder.mountPoint); err != nil || len(entries) != 0 {
		t.Errorf("the image is still mounted: %v, %v", entries, err)
	}
	return build
}

// mountReadOnly mounts the image as a sandbox does.
func mountReadOnly(t *testing.T, device string) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount(device, dir, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, "noload"); err != nil {
		t.Fatalf("mount the image read-only: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(dir, 0); err != nil {
			t.Errorf("unmount the image: %v", err)
		}
	})
	return dir
}

func requireCleanFilesystem(t *testing.T, device string) {
	t.Helper()
	if output, err := exec.Command(e2fsckPath, "-f", "-n", device).CombinedOutput(); err != nil {
		t.Errorf("e2fsck -fn: %v\n%s", err, output)
	}
}

func TestRealBuildWritesTheTarIntoAnImage(t *testing.T) {
	owned := tarFile("owned.txt", "owned")
	owned.hdr.Uid, owned.hdr.Gid, owned.hdr.Uname = 1000, 1000, "user"
	private := tarFile("private.bin", "secret")
	private.hdr.Mode = 0o600
	labelled := tarFile("labelled.txt", "label")
	labelled.hdr.PAXRecords = map[string]string{
		"SCHILY.xattr.user.note":           "note",
		"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\xff\xff\xff\xff",
	}
	restricted := tarDir("docs/")
	restricted.hdr.Mode = 0o700

	tests := map[string]func([]byte) []byte{
		"plain": func(body []byte) []byte { return body },
		"gzip":  func(body []byte) []byte { return gzipped(t, body) },
	}
	for name, encode := range tests {
		t.Run(name, func(t *testing.T) {
			body := encode(buildTar(t,
				restricted,
				tarFile("docs/readme.txt", "read me"),
				tarFile("a/b/c/deep.txt", "deep"),
				tarExecutable("bin/tool", "#!/bin/sh\necho tool\n"),
				tarSymlink("bin/up", "../docs/readme.txt"),
				tarSymlink("absolute", "/etc/hostname"),
				tarFile("empty", ""),
				owned, private, labelled,
			))
			tarPath, params := tarDrive(t, func(out io.Writer) {
				if _, err := out.Write(body); err != nil {
					t.Fatal(err)
				}
			})
			build := runRealBuild(t, tarPath, params, 0, 0)

			if build.status.Result != ResultOK {
				t.Fatalf("status = %+v", build.status)
			}
			if build.status.Files != 7 || build.status.Bytes != params.ContentBytes || build.status.UsedBytes <= 0 {
				t.Errorf("status = %+v, want 7 files and %d bytes", build.status, params.ContentBytes)
			}
			requireCleanFilesystem(t, build.output)

			type node struct {
				mode    fs.FileMode
				content string
			}
			want := map[string]node{
				"lost+found":      {mode: fs.ModeDir | 0o700},
				"docs":            {mode: fs.ModeDir | 0o755},
				"docs/readme.txt": {mode: 0o644, content: "read me"},
				"a":               {mode: fs.ModeDir | 0o755},
				"a/b":             {mode: fs.ModeDir | 0o755},
				"a/b/c":           {mode: fs.ModeDir | 0o755},
				"a/b/c/deep.txt":  {mode: 0o644, content: "deep"},
				"bin":             {mode: fs.ModeDir | 0o755},
				"bin/tool":        {mode: 0o755, content: "#!/bin/sh\necho tool\n"},
				"bin/up":          {mode: fs.ModeSymlink | 0o777, content: "../docs/readme.txt"},
				"absolute":        {mode: fs.ModeSymlink | 0o777, content: "/etc/hostname"},
				"empty":           {mode: 0o644},
				"owned.txt":       {mode: 0o644, content: "owned"},
				"private.bin":     {mode: 0o644, content: "secret"},
				"labelled.txt":    {mode: 0o644, content: "label"},
			}
			mounted := mountReadOnly(t, build.output)
			got := map[string]node{}
			err := filepath.WalkDir(mounted, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(mounted, path)
				info, err := entry.Info()
				if err != nil {
					return err
				}
				stat := info.Sys().(*syscall.Stat_t)
				if stat.Uid != 0 || stat.Gid != 0 {
					t.Errorf("%s is owned by %d:%d, want root", rel, stat.Uid, stat.Gid)
				}
				if size, err := unix.Llistxattr(path, nil); err != nil || size != 0 {
					t.Errorf("%s has extended attributes: %d bytes, %v", rel, size, err)
				}
				if rel == "." {
					if info.Mode() != fs.ModeDir|0o755 {
						t.Errorf("the root has mode %v", info.Mode())
					}
					return nil
				}
				found := node{mode: info.Mode()}
				switch {
				case info.Mode()&fs.ModeSymlink != 0:
					found.content, err = os.Readlink(path)
				case info.Mode().IsRegular():
					var content []byte
					content, err = os.ReadFile(path)
					found.content = string(content)
				}
				got[rel] = found
				return err
			})
			if err != nil {
				t.Fatalf("walk the image: %v", err)
			}
			for path, wantNode := range want {
				if gotNode, ok := got[path]; !ok || gotNode != wantNode {
					t.Errorf("%s = %+v (in the image: %v), want %+v", path, gotNode, ok, wantNode)
				}
			}
			for path := range got {
				if _, ok := want[path]; !ok {
					t.Errorf("%s is in the image, not in the tar", path)
				}
			}
		})
	}
}

func TestRealBuildFitsTheSizedDrive(t *testing.T) {
	const mib = 1 << 20
	longName := strings.Repeat("n", 239)
	tests := map[string]func(add func(tarEntry), large func(name string, size int64)){
		"one empty file": func(add func(tarEntry), _ func(string, int64)) {
			add(tarFile("empty", ""))
		},
		"many tiny files": func(add func(tarEntry), _ func(string, int64)) {
			add(tarDir("tiny/"))
			for index := range 20000 {
				add(tarFile(fmt.Sprintf("tiny/%05d", index), "x"))
			}
		},
		"a directory left out per file": func(add func(tarEntry), _ func(string, int64)) {
			for index := range 5000 {
				add(tarFile(fmt.Sprintf("%04d/file", index), "x"))
			}
		},
		// 600 directories the tar leaves out, more than its entries and the
		// spare: a tar written file by file, without directory entries.
		"a few files below long paths": func(add func(tarEntry), _ func(string, int64)) {
			for index := range 20 {
				add(tarFile(strings.Repeat(fmt.Sprintf("dir-%02d/", index), 30)+"file", "x"))
			}
		},
		"one large file": func(_ func(tarEntry), large func(string, int64)) {
			large("large.bin", 300*mib)
		},
		"files around block sizes": func(add func(tarEntry), _ func(string, int64)) {
			for index := range 600 {
				add(tarFile(fmt.Sprintf("sized-%03d", index), strings.Repeat("s", index*41)))
			}
		},
		"a deep tree": func(add func(tarEntry), _ func(string, int64)) {
			path := ""
			for range 400 {
				path += "deep/"
				add(tarDir(path))
			}
			add(tarFile(path+"bottom", "bottom"))
			// And one whose directories the tar leaves out.
			add(tarFile(strings.Repeat("left-out/", 250)+"bottom", "bottom"))
		},
		"long names": func(add func(tarEntry), _ func(string, int64)) {
			for index := range 3000 {
				add(tarFile(fmt.Sprintf("%s-%015d", longName, index), "x"))
			}
		},
		"long link targets": func(add func(tarEntry), _ func(string, int64)) {
			for index := range 2000 {
				add(tarSymlink(fmt.Sprintf("link-%04d", index), "../"+longName))
			}
		},
	}
	for name, fill := range tests {
		t.Run(name, func(t *testing.T) {
			tarPath, params := tarDrive(t, func(out io.Writer) {
				tw := tar.NewWriter(out)
				add := func(entry tarEntry) {
					hdr := entry.hdr
					hdr.Size = int64(len(entry.body))
					if err := tw.WriteHeader(&hdr); err != nil {
						t.Fatal(err)
					}
					if _, err := io.WriteString(tw, entry.body); err != nil {
						t.Fatal(err)
					}
				}
				large := func(name string, size int64) {
					if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
						t.Fatal(err)
					}
					if _, err := io.CopyN(tw, patternReader{}, size); err != nil {
						t.Fatal(err)
					}
				}
				fill(add, large)
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
			})
			build := runRealBuild(t, tarPath, params, 0, 0)
			if build.status.Result != ResultOK {
				t.Fatalf("status = %+v for %+v on %d bytes", build.status, params, ImageBytes(params))
			}
			requireCleanFilesystem(t, build.output)

			var stat unix.Statfs_t
			if err := unix.Statfs(mountReadOnly(t, build.output), &stat); err != nil {
				t.Fatal(err)
			}
			t.Logf("%d entries, %d content bytes: image %d KiB, %d KiB free; %d inodes, %d free",
				params.Entries, params.ContentBytes, ImageBytes(params)>>10, stat.Bfree*uint64(stat.Bsize)>>10, stat.Files, stat.Ffree)
		})
	}
}

// patternReader is content that is not all zeros.
type patternReader struct{}

func (patternReader) Read(buf []byte) (int, error) {
	for index := range buf {
		buf[index] = byte(index)
	}
	return len(buf), nil
}

func TestRealBuildFailures(t *testing.T) {
	hundredFiles := func() []tarEntry {
		entries := make([]tarEntry, 100)
		for index := range entries {
			entries[index] = tarFile(fmt.Sprintf("file-%03d", index), "x")
		}
		return entries
	}
	megabytes := func(count int) tarEntry {
		return tarFile("content.bin", strings.Repeat("c", count<<20))
	}

	tests := []struct {
		name        string
		entries     []tarEntry
		change      func(*Params)
		outputBytes int64
		inodes      int64
		want        string
	}{
		{name: "hard link", entries: []tarEntry{tarFile("a", "x"), tarHardLink("b", "a")},
			change: func(params *Params) { params.Entries, params.ContentBytes = 2, 1 }, want: ReasonTar},
		{name: "output drive too small", entries: []tarEntry{megabytes(8)}, outputBytes: 4 << 20, want: ReasonNoSpace},
		{name: "too few inodes", entries: hundredFiles(), inodes: 16, want: ReasonNoSpace},
		{name: "more entries than counted", entries: hundredFiles(), change: func(params *Params) { params.Entries-- }, want: ReasonTooLarge},
		{name: "more content than counted", entries: []tarEntry{megabytes(1)}, change: func(params *Params) { params.ContentBytes-- }, want: ReasonTooLarge},
		{name: "more decompressed than counted", entries: []tarEntry{megabytes(1)}, change: func(params *Params) { params.DecompressedBytes-- }, want: ReasonTooLarge},
		{name: "a path longer than a system call takes", entries: []tarEntry{tarDir(strings.Repeat("d/", 2048))},
			change: func(params *Params) { params.Entries = 1 }, want: ReasonTar},
		{name: "a file below a file", entries: []tarEntry{tarFile("a", "x"), tarFile("a/b", "y")}, want: ReasonTar},
		{name: "a name longer than ext4 takes", entries: []tarEntry{tarFile(strings.Repeat("n", 256), "x")}, want: ReasonTar},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tarPath, params := entriesDrive(t, tt.entries...)
			if tt.change != nil {
				tt.change(&params)
			}
			build := runRealBuild(t, tarPath, params, tt.outputBytes, tt.inodes)
			if build.status.Result != ResultFailed || build.status.Reason != tt.want {
				t.Errorf("status = %+v, want reason %q", build.status, tt.want)
			}
		})
	}
}

func TestToolOutputKeepsOnlyItsEnd(t *testing.T) {
	var output toolOutput
	for range 1000 {
		if written, err := output.Write([]byte(strings.Repeat("x", 99) + "\n")); written != 100 || err != nil {
			t.Fatalf("Write() = %d, %v", written, err)
		}
	}
	_, _ = output.Write([]byte("the last line"))
	if len(output.tail) != maxToolOutputBytes || !strings.HasSuffix(string(output.tail), "x\nthe last line") {
		t.Errorf("kept %d bytes ending in %q", len(output.tail), output.tail[len(output.tail)-20:])
	}

	flood := strings.Repeat("y", 3*maxToolOutputBytes) + "the end of one write"
	if written, err := output.Write([]byte(flood)); written != len(flood) || err != nil {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if len(output.tail) != maxToolOutputBytes || !strings.HasSuffix(string(output.tail), "ythe end of one write") {
		t.Errorf("kept %d bytes ending in %q", len(output.tail), output.tail[len(output.tail)-20:])
	}
}

func TestRealTarRefusesAShortDrive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tar")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := System().Tar(path, 1025); err == nil {
		t.Error("a tar longer than its drive was opened")
	}
	tar, err := System().Tar(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer tar.Close()
	if read, err := io.Copy(io.Discard, tar); err != nil || read != 1000 {
		t.Errorf("read %d bytes, %v; want the tar's 1000", read, err)
	}
}
