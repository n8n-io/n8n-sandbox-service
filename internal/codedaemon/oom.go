// This file reads the Linux cgroup v2 OOM kill counter for runtime exit classification.
package codedaemon

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// oomKillCount returns the cgroup-wide count, or -1 if the counter is unavailable.
func oomKillCount(dir string) int64 {
	if dir == "" {
		return -1
	}
	f, err := os.Open(filepath.Join(dir, "memory.events"))
	if err != nil {
		return -1
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if value, ok := strings.CutPrefix(s.Text(), "oom_kill "); ok {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
				return n
			}
			break
		}
	}
	return -1
}
