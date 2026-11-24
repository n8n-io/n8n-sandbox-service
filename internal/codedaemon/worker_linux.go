// This file watches for the runtime process to exit on Linux using waitid.

package codedaemon

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Observe the exit without reaping the child. If observing fails, the
// caller still terminates the group before reaping its leader.
func waitWorkerExit(pid int) {
	var info unix.Siginfo
	for errors.Is(unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil), unix.EINTR) {
	}
}
