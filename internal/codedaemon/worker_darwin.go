// This file watches for the runtime process to exit on macOS using kqueue.

package codedaemon

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Observe the exit without reaping the child. If observing fails, the
// caller still terminates the group before reaping its leader.
func waitWorkerExit(pid int) {
	syscall.ForkLock.RLock()
	kq, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return
	}
	defer unix.Close(kq)
	change := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	var event [1]unix.Kevent_t
	for _, err := unix.Kevent(kq, change, event[:], nil); err == unix.EINTR; _, err = unix.Kevent(kq, change, event[:], nil) {
	}
}
