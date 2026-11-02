//go:build linux

package firecracker

import (
	"context"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// dialContextInNetNS dials the guest address from inside netnsPath on an OS
// thread of its own, because Linux network namespaces are thread-local.
func dialContextInNetNS(ctx context.Context, netnsPath string, network string, address string) (net.Conn, error) {
	resultCh := make(chan netnsDialResult, 1)
	go func() {
		conn, err := dialContextInNetNSOnLockedThread(ctx, netnsPath, network, address)
		resultCh <- netnsDialResult{conn: conn, err: err}
	}()

	result := <-resultCh
	return result.conn, result.err
}

type netnsDialResult struct {
	conn net.Conn
	err  error
}

// The thread stays locked, so Go ends it with the goroutine rather than reuse
// it in a sandbox namespace.
func dialContextInNetNSOnLockedThread(ctx context.Context, netnsPath string, network string, address string) (net.Conn, error) {
	runtime.LockOSThread()

	// Not /proc/self, which is the main thread.
	current, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		return nil, err
	}
	defer current.Close()

	target, err := os.Open(netnsPath)
	if err != nil {
		return nil, err
	}
	defer target.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, err
	}
	// Go parks the main thread instead of ending it, so switch back.
	defer unix.Setns(int(current.Fd()), unix.CLONE_NEWNET)

	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}
