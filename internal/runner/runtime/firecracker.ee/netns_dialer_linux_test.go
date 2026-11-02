//go:build linux

package firecracker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// Keeps the main goroutine on the main thread, so tests can run code there.
func init() {
	runtime.LockOSThread()
}

var mainThreadFuncs = make(chan func())

func TestMain(m *testing.M) {
	done := make(chan int)
	go func() { done <- m.Run() }()
	for {
		select {
		case f := <-mainThreadFuncs:
			f()
		case code := <-done:
			os.Exit(code)
		}
	}
}

func onMainThread(f func()) {
	done := make(chan struct{})
	mainThreadFuncs <- func() { defer close(done); f() }
	<-done
}

// The runner's main thread can sit inside a sandbox namespace for a dial of its
// own while other dials run. No thread may be left in that namespace afterwards.
func TestDialContextInNetNSLeavesThreadsInHostNamespace(t *testing.T) {
	host, err := os.Open("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	hostNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}

	onMainThread(func() { err = unix.Unshare(unix.CLONE_NEWNET) })
	if errors.Is(err, unix.EPERM) {
		t.Skip("needs CAP_SYS_ADMIN")
	}
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			// Dials the main thread's namespace; lo is down there, so the dial fails.
			if conn, err := dialContextInNetNS(context.Background(), "/proc/self/ns/net", "tcp", "127.0.0.1:1"); err == nil {
				conn.Close()
			}
		})
	}
	wg.Wait()

	onMainThread(func() { err = unix.Setns(int(host.Fd()), unix.CLONE_NEWNET) })
	if err != nil {
		t.Fatal(err)
	}

	links, err := filepath.Glob("/proc/self/task/*/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		ns, err := os.Readlink(link)
		if errors.Is(err, fs.ErrNotExist) {
			continue // the thread exited
		}
		if err != nil {
			t.Fatal(err)
		}
		if ns != hostNS {
			t.Errorf("%s = %s, want host namespace %s", link, ns, hostNS)
		}
	}
}
