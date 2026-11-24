// Package hostinfo reads the memory and disk size of the machine a runner is on.
package hostinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// MeminfoPath is where the kernel reports host memory.
const MeminfoPath = "/proc/meminfo"

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

// VolumeTotal returns the total size, in bytes, of the filesystem holding path.
func VolumeTotal(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("read size of %s: %w", path, err)
	}
	return int64(stat.Blocks) * int64(stat.Bsize), nil
}
