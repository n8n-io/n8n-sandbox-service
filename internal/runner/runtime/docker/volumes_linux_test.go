package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// mountsAt counts the mounts at path in this process's mount table.
func mountsAt(t *testing.T, path string) int {
	t.Helper()
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for line := range strings.SplitSeq(string(mountinfo), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == path {
			count++
		}
	}
	return count
}

// The cache dir becomes nosuid,nodev and keeps the flags it had; a second
// call, as from a runner restarted in the same container, mounts nothing.
func TestSetNoSuidNoDevKeepsTheMountsFlags(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOEXEC|unix.MS_NOATIME, "size=1m"); err != nil {
		t.Skipf("needs root that may mount: %v", err)
	}
	t.Cleanup(func() {
		for mountsAt(t, dir) > 0 {
			if err := unix.Unmount(dir, unix.MNT_DETACH); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "before"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if checkNoSuidNoDev(dir) == nil {
		t.Fatal("a fresh tmpfs is nosuid,nodev already")
	}

	if err := setNoSuidNoDev(dir); err != nil {
		t.Fatalf("setNoSuidNoDev() failed: %v", err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	want := int64(unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC | unix.ST_NOATIME)
	if int64(stat.Flags)&want != want {
		t.Errorf("flags = %#x, want %#x set", stat.Flags, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "before")); err != nil {
		t.Errorf("content gone after the bind: %v", err)
	}
	mounts := mountsAt(t, dir)
	if err := setNoSuidNoDev(dir); err != nil {
		t.Fatal(err)
	}
	if again := mountsAt(t, dir); again != mounts {
		t.Errorf("a second call left %d mounts at the dir, want %d", again, mounts)
	}
}

// A strictatime mount stays strictatime: a user namespace may lock it.
func TestSetNoSuidNoDevKeepsStrictAtime(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_STRICTATIME, "size=1m"); err != nil {
		t.Skipf("needs root that may mount: %v", err)
	}
	t.Cleanup(func() {
		for mountsAt(t, dir) > 0 {
			if err := unix.Unmount(dir, unix.MNT_DETACH); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := setNoSuidNoDev(dir); err != nil {
		t.Fatalf("setNoSuidNoDev() failed: %v", err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	if int64(stat.Flags)&(unix.ST_RELATIME|unix.ST_NOATIME) != 0 {
		t.Errorf("flags = %#x, want strictatime kept", stat.Flags)
	}
}

func TestSetNoSuidNoDevRefusesNoDir(t *testing.T) {
	if err := setNoSuidNoDev(""); err == nil {
		t.Fatal("setNoSuidNoDev(\"\") succeeded")
	}
	if err := setNoSuidNoDev(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("setNoSuidNoDev() of a missing dir succeeded")
	}
}
