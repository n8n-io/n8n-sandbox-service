//go:build linux

package volumebuild

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// Absolute paths: init gets no PATH from the kernel. Neither tool needs /proc,
// /sys or a writable directory.
const (
	mke2fsPath = "/sbin/mke2fs"
	e2fsckPath = "/sbin/e2fsck"
)

// System is the real system calls and tools.
func System() Sys {
	return linuxSys{}
}

type linuxSys struct{}

func (linuxSys) Pid() int  { return os.Getpid() }
func (linuxSys) Euid() int { return os.Geteuid() }

func (linuxSys) Tar(device string, length int64) (io.ReadCloser, error) {
	file, err := os.Open(device)
	if err != nil {
		return nil, err
	}
	size, err := file.Seek(0, io.SeekEnd)
	if err == nil && size < length {
		err = fmt.Errorf("%s has %d bytes, fewer than the tar's %d", device, size, length)
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(bufio.NewReaderSize(file, 1<<20), length), file}, nil
}

func (linuxSys) Format(device string, inodes int64) error {
	err := runTool(mke2fsPath, formatArgs(device, inodes, "^has_journal,^resize_inode,^orphan_file")...)
	if err == nil {
		return nil
	}
	// A mke2fs from before orphan_file refuses the name.
	if runTool(mke2fsPath, formatArgs(device, inodes, "^has_journal,^resize_inode")...) == nil {
		return nil
	}
	return err
}

// formatArgs fixes everything the sizing rule in size.go relies on.
// lazy_itable_init=0 and nodiscard have mke2fs zero the inode tables itself,
// whatever the drive held and claims.
func formatArgs(device string, inodes int64, features string) []string {
	return []string{
		"-q", "-F", "-t", "ext4",
		"-b", strconv.Itoa(blockBytes),
		"-I", strconv.Itoa(inodeBytes),
		"-N", strconv.FormatInt(inodes, 10),
		"-m", "0",
		"-O", features,
		"-E", "root_owner=0:0,lazy_itable_init=0,nodiscard",
		device,
	}
}

func (linuxSys) Mount(device, path string) error {
	// Unpacked files and directories get their modes less the umask.
	unix.Umask(0o022)
	return unix.Mount(device, path, "ext4", unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOEXEC, "")
}

func (linuxSys) UsedBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
}

func (linuxSys) Flush(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Syncfs(int(dir.Fd()))
}

// Unmount is not lazy: e2fsck opens the drive exclusively.
func (linuxSys) Unmount(path string) error {
	return unix.Unmount(path, 0)
}

func (linuxSys) Check(device string) error {
	return runTool(e2fsckPath, "-f", "-n", device)
}

func (linuxSys) WriteStatus(device string, record []byte) error {
	file, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.WriteAt(record, 0)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Reboot restarts the guest, which booted with reboot=k: Firecracker exits.
func (linuxSys) Reboot() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}

const maxToolOutputBytes = 4 << 10

// toolOutput keeps the end of what a tool prints: init has little memory, and
// e2fsck prints a line for every problem it finds.
type toolOutput struct {
	tail []byte
}

func (o *toolOutput) Write(output []byte) (int, error) {
	o.tail = append(o.tail, output...)
	if extra := len(o.tail) - maxToolOutputBytes; extra > 0 {
		o.tail = o.tail[:copy(o.tail, o.tail[extra:])]
	}
	return len(output), nil
}

// runTool fails unless the tool exits with 0. The end of its output goes into
// the error.
func runTool(path string, args ...string) error {
	var output toolOutput
	cmd := exec.Command(path, args...)
	cmd.Env = []string{}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", path, err, bytes.TrimSpace(output.tail))
	}
	return nil
}
