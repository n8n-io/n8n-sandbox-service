package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// HandleExec runs a command inside workdir with the given environment. The
// command is always executed via /bin/sh -c so that shell features like tilde
// expansion, pipes, and redirects work consistently.
//
// It streams stdout and stderr lines to callback as Response messages, and sends
// a final "exit" response with metadata (success, executionTimeMs, timedOut, killed).
//
// If ctx is cancelled, the entire process group is killed before returning.
func HandleExec(ctx context.Context, command string, env []string, workdir string, callback func(Response)) error {
	const pipeDrainGrace = 250 * time.Millisecond

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)

	// Start with sensible defaults so common tools work out of the box, then
	// layer caller-supplied env vars on top (later values win).
	cmd.Env = append([]string{
		"HOME=/home/user",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}, env...)

	if workdir != "" {
		cmd.Dir = workdir
	}

	// Put the child in its own process group so we can kill the whole tree.
	// The daemon normally drops to the sandbox user during startup; the
	// credential fallback keeps direct root-started guests safe too.
	cmd.SysProcAttr = commandSysProcAttr()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	startTime := time.Now()

	slog.Info("exec start", "command", command, "workdir", workdir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	pgid := cmd.Process.Pid // Setpgid: true → pgid == pid

	// processDone is closed once the shell process has been reaped, signalling
	// the kill goroutine that the process has already exited.
	processDone := make(chan struct{})

	// Kill the entire process group when ctx is cancelled.
	go func() {
		select {
		case <-ctx.Done():
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
				slog.Warn("kill process group", "pgid", pgid, "err", err)
			}
		case <-processDone:
			// Process finished on its own; nothing to kill.
		}
	}()

	// Stream stdout.
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			callback(Response{Type: ResponseTypeStdout, Data: scanner.Text() + "\n"})
		}
		if err := scanner.Err(); err != nil {
			slog.Warn("scan stdout", "err", err)
		}
	}()

	// Stream stderr.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			callback(Response{Type: ResponseTypeStderr, Data: scanner.Text() + "\n"})
		}
		if err := scanner.Err(); err != nil {
			slog.Warn("scan stderr", "err", err)
		}
	}()

	processState, waitErr := cmd.Process.Wait()
	close(processDone) // unblock the kill goroutine if ctx was not cancelled

	// Reap the shell process first, then give stdout/stderr readers a brief
	// window to drain buffered output before force-closing inherited pipes from
	// background children that would otherwise keep the exec open indefinitely.
	drainReaders := func(done <-chan struct{}, pipe interface{ Close() error }) {
		select {
		case <-done:
		case <-time.After(pipeDrainGrace):
			_ = pipe.Close()
			<-done
		}
	}
	drainReaders(stdoutDone, stdoutPipe)
	drainReaders(stderrDone, stderrPipe)

	// Finalize exec.Cmd bookkeeping after reads have completed. Because the
	// process was already reaped via Process.Wait, cmd.Wait may report ECHILD;
	// treat that as expected rather than surfacing a spurious exec failure.
	if err := finalizeCmdWait(cmd); err != nil {
		slog.Warn("finalize command wait", "err", err)
	}

	executionTimeMs := time.Since(startTime).Milliseconds()

	exitCode := 0
	timedOut := false
	killed := false

	if processState != nil {
		exitCode = processState.ExitCode()
		if status, ok := processState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			killed = true
		}
	}
	if waitErr != nil {
		exitCode = -1
		killed = true
		slog.Debug("exec wait error", "err", waitErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		timedOut = true
		killed = true
	}

	success := exitCode == 0

	slog.Info("exec done",
		"command", command,
		"exit_code", exitCode,
		"duration_ms", executionTimeMs,
		"timed_out", timedOut,
		"killed", killed,
	)

	callback(Response{
		Type:            ResponseTypeExit,
		ExitCode:        exitCode,
		Success:         &success,
		ExecutionTimeMs: executionTimeMs,
		TimedOut:        &timedOut,
		Killed:          &killed,
	})
	return nil
}

func finalizeCmdWait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	if err == nil {
		return nil
	}

	// The process was already reaped via cmd.Process.Wait(), so cmd.Wait()
	// may return os.ErrProcessDone (Go 1.20+ with pidfd) or ECHILD
	// (traditional wait4). Both are expected.
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) && errors.Is(syscallErr.Err, syscall.ECHILD) {
		return nil
	}
	return err
}
