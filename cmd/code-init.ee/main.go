//go:build linux

// Command code-init is PID 1 of a Firecracker code sandbox (Task API). It
// mounts the kernel file systems, puts the code daemon in a cgroup of its own
// (so the daemon reports CPU and memory use as in a Docker code sandbox),
// starts the daemon as uid 1000 and reaps orphaned processes. It exits when the
// daemon exits, which ends the guest (panic=1): a code sandbox whose daemon
// died is gone.
//
// The daemon settings are KEY=VALUE lines in /etc/code-daemon.env, written into
// the rootfs when the golden code snapshot is built.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	daemonBin  = "/usr/local/bin/code-daemon"
	envFile    = "/etc/code-daemon.env"
	cgroupRoot = "/sys/fs/cgroup"
	cgroupDir  = cgroupRoot + "/code"
	sandboxUID = 1000
	sandboxGID = 1000

	prSetNoNewPrivs = 38
)

func main() {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "code-init: "+format+"\n", args...)
	}
	if err := mountAll(); err != nil {
		logf("mount: %v", err)
		os.Exit(1)
	}
	_ = syscall.Sethostname([]byte("sandbox"))
	// build-rootfs-template.sh makes the tree root-owned. The daemon (uid 1000)
	// writes task files to its work dir.
	for _, dir := range []string{"/work", "/home/node"} {
		if err := chownTree(dir); err != nil {
			logf("chown %s: %v", dir, err)
		}
	}
	if err := setupCgroup(); err != nil {
		// The daemon still runs. It reports zero CPU and memory use.
		logf("cgroup: %v", err)
	}
	env, err := daemonEnv()
	if err != nil {
		logf("env: %v", err)
		os.Exit(1)
	}
	// As no-new-privileges in a Docker code sandbox: the daemon and the task
	// processes inherit it, so setuid binaries cannot give them root.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		logf("no_new_privs: %v", errno)
		os.Exit(1)
	}
	pid, err := startDaemon(env)
	if err != nil {
		logf("start daemon: %v", err)
		os.Exit(1)
	}
	logf("code daemon started, pid %d", pid)
	status := reap(pid)
	logf("code daemon exited: %v", status)
	os.Exit(1)
}

type mountSpec struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

func mountAll() error {
	mounts := []mountSpec{
		{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"},
		{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755"},
		{"cgroup2", cgroupRoot, "cgroup2", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data)
		// The kernel mounts devtmpfs itself with CONFIG_DEVTMPFS_MOUNT.
		if err != nil && !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	return nil
}

func chownTree(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, sandboxUID, sandboxGID)
	})
}

func setupCgroup() error {
	if err := os.WriteFile(cgroupRoot+"/cgroup.subtree_control", []byte("+cpu +memory"), 0o644); err != nil {
		return fmt.Errorf("enable controllers: %w", err)
	}
	if err := os.MkdirAll(cgroupDir, 0o755); err != nil {
		return err
	}
	return nil
}

func daemonEnv() ([]string, error) {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/home/node",
		"CODE_DAEMON_CGROUP_DIR=" + cgroupDir,
	}
	f, err := os.Open(envFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			return nil, fmt.Errorf("invalid line in %s: %q", envFile, line)
		}
		env = append(env, line)
	}
	return env, sc.Err()
}

// startDaemon starts the daemon as uid 1000 in the code cgroup. Its output goes
// to /dev/null: the serial console is slow and blocks the guest.
func startDaemon(env []string) (int, error) {
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()
	cred := &syscall.Credential{Uid: sandboxUID, Gid: sandboxGID, Groups: []uint32{}}
	attr := &syscall.ProcAttr{
		Dir:   "/work",
		Env:   env,
		Files: []uintptr{devNull.Fd(), devNull.Fd(), devNull.Fd()},
		Sys:   &syscall.SysProcAttr{Credential: cred, Setsid: true},
	}
	if cg, err := os.Open(cgroupDir); err == nil {
		defer cg.Close()
		attr.Sys.UseCgroupFD = true
		attr.Sys.CgroupFD = int(cg.Fd())
		pid, err := syscall.ForkExec(daemonBin, []string{daemonBin}, attr)
		if err == nil {
			return pid, nil
		}
		attr.Sys.UseCgroupFD = false
	}
	pid, err := syscall.ForkExec(daemonBin, []string{daemonBin}, attr)
	if err != nil {
		return 0, err
	}
	// Fallback for a kernel without CLONE_INTO_CGROUP: move the daemon after the
	// start. Its workers start later, so they are in the cgroup too.
	_ = os.WriteFile(cgroupDir+"/cgroup.procs", []byte(strconv.Itoa(pid)), 0o644)
	return pid, nil
}

// reap waits for all children (orphans are re-parented to PID 1) and returns
// when the daemon exits.
func reap(daemonPID int) syscall.WaitStatus {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return ws
		}
		if pid == daemonPID {
			return ws
		}
	}
}
