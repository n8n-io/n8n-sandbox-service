package daemon

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// reapedTTL bounds how long the exit status of a child nobody waits for is
// kept. It outlasts the gap between starting a command and waiting on it.
const reapedTTL = 30 * time.Second

// reaper is the daemon's wait4(-1) loop. The daemon is PID 1 in a sandbox, so
// every orphan of every command reparents to it and stays a zombie, eating
// the sandbox's pids limit, until something waits on it. Because the loop
// reaps every child, including the ones commands start, the daemon gets their
// exit status from wait rather than from os.Process.Wait.
type reaper struct {
	mu      sync.Mutex
	exited  map[int]reapedChild
	waiters map[int]chan syscall.WaitStatus
}

type reapedChild struct {
	status syscall.WaitStatus
	at     time.Time
}

// activeReaper is set while the daemon runs as PID 1.
var activeReaper *reaper

// startReaper starts the daemon's reaper when it is PID 1, where it is the
// init that orphans reparent to.
func startReaper() {
	if os.Getpid() != 1 || activeReaper != nil {
		return
	}
	activeReaper = newReaper()
	go activeReaper.run()
}

func newReaper() *reaper {
	return &reaper{
		exited:  make(map[int]reapedChild),
		waiters: make(map[int]chan syscall.WaitStatus),
	}
}

func (r *reaper) run() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	// SIGCHLD signals coalesce, so a pass reaps until none is left, and a
	// tick covers a signal that arrives mid-pass.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		r.reapAll()
		select {
		case <-sigs:
		case <-tick.C:
		}
	}
}

func (r *reaper) reapAll() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			break
		}
		r.record(pid, status)
	}
	r.prune()
}

func (r *reaper) record(pid int, status syscall.WaitStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.waiters[pid]; ok {
		delete(r.waiters, pid)
		ch <- status
		return
	}
	r.exited[pid] = reapedChild{status: status, at: time.Now()}
}

func (r *reaper) prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, c := range r.exited {
		if time.Since(c.at) > reapedTTL {
			delete(r.exited, pid)
		}
	}
}

// wait returns the exit status of child pid once the loop has reaped it.
func (r *reaper) wait(pid int) syscall.WaitStatus {
	r.mu.Lock()
	if c, ok := r.exited[pid]; ok {
		delete(r.exited, pid)
		r.mu.Unlock()
		return c.status
	}
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = ch
	r.mu.Unlock()
	return <-ch
}

// waitProcess waits for p, returning its exit code (-1 when a signal ended
// it) and whether a signal did.
func waitProcess(p *os.Process) (exitCode int, signaled bool, err error) {
	if r := activeReaper; r != nil {
		status := r.wait(p.Pid)
		if status.Signaled() {
			return -1, true, nil
		}
		return status.ExitStatus(), false, nil
	}
	state, err := p.Wait()
	if err != nil {
		return -1, false, err
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return state.ExitCode(), ok && status.Signaled(), nil
}

// logReaperStart is for the daemon's startup log.
func logReaperStart() {
	if activeReaper != nil {
		slog.Info("reaping orphaned processes as PID 1")
	}
}
