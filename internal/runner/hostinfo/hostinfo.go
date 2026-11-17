// Package hostinfo reads the memory and disk size of the machine a runner is on.
package hostinfo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// MeminfoPath is where the kernel reports host memory.
	MeminfoPath = "/proc/meminfo"
	// SelfCgroupPath names the cgroup this process is in.
	SelfCgroupPath = "/proc/self/cgroup"
	// CgroupRoot is where the cgroup v2 hierarchy is mounted.
	CgroupRoot = "/sys/fs/cgroup"
)

// MemTotal returns MemTotal from a /proc/meminfo-formatted file, in bytes.
func MemTotal(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("read host memory: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("read host memory: unexpected MemTotal line %q in %s", sc.Text(), path)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0, fmt.Errorf("read host memory: unexpected MemTotal line %q in %s", sc.Text(), path)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read host memory: %w", err)
	}
	return 0, fmt.Errorf("read host memory: no MemTotal in %s", path)
}

// CgroupMemoryLimit returns the finite memory.max of this process's cgroup v2
// (the "0::<path>" line of selfCgroup, under root). ok is false for anything
// else: no such line, a ".." path, no file, "max", or cgroup v1.
func CgroupMemoryLimit(selfCgroup, root string) (limit int64, ok bool, err error) {
	raw, err := os.ReadFile(selfCgroup)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read cgroup membership: %w", err)
	}
	var rel string
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if p, cut := strings.CutPrefix(line, "0::"); cut {
			rel, found = strings.TrimSpace(p), true
			break
		}
	}
	root = filepath.Clean(root)
	dir := filepath.Clean(filepath.Join(root, rel))
	if !found || !strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || !withinDir(root, dir) {
		return 0, false, nil
	}
	var tightest int64
	var have bool
	for {
		n, finite, err := readMemoryMax(filepath.Join(dir, "memory.max"))
		if err != nil {
			return 0, false, err
		}
		if finite && (!have || n < tightest) {
			tightest, have = n, true
		}
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !withinDir(root, parent) {
			break
		}
		dir = parent
	}
	return tightest, have, nil
}

// withinDir reports whether path is dir or a directory inside it.
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readMemoryMax(path string) (limit int64, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read cgroup memory limit: %w", err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("read cgroup memory limit: unexpected value %q in %s", v, path)
	}
	return n, true, nil
}

// VolumeTotal returns the total size, in bytes, of the filesystem holding path.
func VolumeTotal(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("read size of %s: %w", path, err)
	}
	return int64(stat.Blocks) * int64(stat.Bsize), nil
}
