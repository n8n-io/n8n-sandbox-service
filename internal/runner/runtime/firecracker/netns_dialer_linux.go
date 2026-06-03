//go:build linux

package firecracker

import (
	"context"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// dialContextInNetNS temporarily moves the current OS thread into netnsPath,
// dials the guest address from there, and restores the original namespace.
// The thread is locked because Linux network namespaces are thread-local.
func dialContextInNetNS(ctx context.Context, netnsPath string, network string, address string) (net.Conn, error) {
	current, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	defer current.Close()

	target, err := os.Open(netnsPath)
	if err != nil {
		return nil, err
	}
	defer target.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, err
	}
	defer func() {
		_ = unix.Setns(int(current.Fd()), unix.CLONE_NEWNET)
	}()

	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}
