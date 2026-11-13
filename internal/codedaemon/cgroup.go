package codedaemon

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readCgroupUsage reads the CPU time (usec) and the peak and current memory
// (bytes) of a cgroup v2 folder. In a container with a private cgroup
// namespace, /sys/fs/cgroup is the cgroup of the container. A value that cannot
// be read is 0.
func readCgroupUsage(dir string) (cpuUsec, memPeak, memCurrent int64) {
	if dir == "" {
		return 0, 0, 0
	}
	if f, err := os.Open(filepath.Join(dir, "cpu.stat")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "usage_usec "); ok {
				cpuUsec, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				break
			}
		}
		f.Close()
	}
	memPeak = readIntFile(filepath.Join(dir, "memory.peak"))
	memCurrent = readIntFile(filepath.Join(dir, "memory.current"))
	return cpuUsec, memPeak, memCurrent
}

func readIntFile(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}
