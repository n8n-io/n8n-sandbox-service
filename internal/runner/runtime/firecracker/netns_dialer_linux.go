//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// dialContextInNetNS temporarily moves the current OS thread into netnsPath,
// dials the guest address from there, and restores the original namespace.
// The thread is locked because Linux network namespaces are thread-local.
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

// Run namespace switching on one locked OS thread because Linux network
// namespaces are thread-local, not goroutine-local.
func dialContextInNetNSOnLockedThread(ctx context.Context, netnsPath string, network string, address string) (conn net.Conn, err error) {
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
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, err
	}
	defer func() {
		if restoreErr := unix.Setns(int(current.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
			unlockThread = false
			if conn != nil {
				_ = conn.Close()
				conn = nil
			}
			err = errors.Join(err, fmt.Errorf("restore network namespace: %w", restoreErr))
		}
	}()

	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}
