//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// listenInNetNS opens a listener inside netnsPath. A socket stays in the
// namespace it was created in, so once open it serves that namespace alone.
func listenInNetNS(netnsPath, network, address string) (net.Listener, error) {
	type result struct {
		ln  net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ln, err := listenInNetNSOnLockedThread(netnsPath, network, address)
		ch <- result{ln, err}
	}()
	r := <-ch
	return r.ln, r.err
}

func listenInNetNSOnLockedThread(netnsPath, network, address string) (ln net.Listener, err error) {
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
			// The thread is stuck in the sandbox's namespace: never reuse it.
			unlockThread = false
			if ln != nil {
				_ = ln.Close()
				ln = nil
			}
			err = errors.Join(err, fmt.Errorf("restore network namespace: %w", restoreErr))
		}
	}()
	return net.Listen(network, address)
}
