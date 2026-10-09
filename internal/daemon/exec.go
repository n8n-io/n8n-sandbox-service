package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf8"
)

// streamChunkBytes is the read buffer size for stdout and stderr. A line longer
// than this is sent as several events of about this size, which clients join
// back together.
const streamChunkBytes = 64 * 1024

// HandleExec runs a command inside workdir with the given environment. The
// command is always executed via /bin/sh -c so that shell features like tilde
// expansion, pipes, and redirects work consistently.
//
// It streams stdout and stderr lines to callback as Response messages (see
// streamLines for lines longer than streamChunkBytes), and sends
// a final "exit" response with metadata (success, executionTimeMs, timedOut, killed).
//
// If ctx is cancelled, the entire process group is killed before returning.
func HandleExec(ctx context.Context, command string, env []string, workdir string, callback func(Response)) error {
	const pipeDrainGrace = 250 * time.Millisecond

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)

	// Start with sensible defaults so common tools work out of the box, then
	// layer caller-supplied env vars on top (later values win).
	//
	// This replaces the environment rather than extending it, so it is the only
	// one a user command sees on either runtime: image ENV and the runner's
	// --env reach the daemon, not the commands it runs.
	//
	// The first two PATH entries are the install roots a sandbox without root
	// can write: ~/.npm-global for a global npm install, and the prebuilt venv
	// for pip.
	cmd.Env = append([]string{
		"HOME=/home/user",
		"PATH=/home/user/venv/bin:/home/user/.npm-global/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
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
		err := streamLines(stdoutPipe, func(data string) {
			callback(Response{Type: ResponseTypeStdout, Data: data})
		})
		if err != nil {
			slog.Warn("read stdout", "err", err)
		}
	}()

	// Stream stderr.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		err := streamLines(stderrPipe, func(data string) {
			callback(Response{Type: ResponseTypeStderr, Data: data})
		})
		if err != nil {
			slog.Warn("read stderr", "err", err)
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

// streamLines sends each line from r to emit, with a trailing newline. A line
// longer than streamChunkBytes goes out in several chunks, so no output is
// dropped and memory stays bounded.
func streamLines(r io.Reader, emit func(string)) error {
	reader := bufio.NewReaderSize(r, streamChunkBytes)
	var carry []byte
	// midLine is true once part of the current line has been emitted.
	midLine := false
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			if len(carry) > 0 || midLine {
				emit(string(carry) + "\n")
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		// ReadLine reuses its buffer, so append copies chunk into carry.
		data := append(carry, chunk...)
		if !isPrefix {
			carry = nil
			midLine = false
			emit(string(data) + "\n")
			continue
		}

		// Hold back a split UTF-8 character: JSON encoding would replace
		// each half of it with U+FFFD.
		cut := incompleteRuneStart(data)
		carry = append([]byte(nil), data[cut:]...)
		if cut > 0 {
			emit(string(data[:cut]))
			midLine = true
		}
	}
}

// incompleteRuneStart returns the index where an incomplete UTF-8 character at
// the end of b starts, or len(b) if b ends with a complete character.
func incompleteRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if utf8.FullRune(b[i:]) {
			return len(b)
		}
		return i
	}
	return len(b)
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
