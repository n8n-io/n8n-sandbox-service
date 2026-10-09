package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleExecReturnsPromptlyForBackgroundCommand(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var exit Response
	start := time.Now()
	err := HandleExec(ctx, "sleep 30 &", nil, "", func(resp Response) {
		if resp.Type == ResponseTypeExit {
			exit = resp
		}
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("HandleExec() error = %v", err)
	}

	if elapsed >= time.Second {
		t.Fatalf("HandleExec() took %v, expected background command to return promptly", elapsed)
	}
	if exit.Type != ResponseTypeExit {
		t.Fatal("expected exit response")
	}
	if exit.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exit.ExitCode)
	}
	if exit.Killed != nil && *exit.Killed {
		t.Fatal("expected background command wrapper to exit cleanly")
	}
}

func TestHandleExecPreservesStdoutForBackgroundCommand(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var stdout string
	var exit Response
	start := time.Now()
	err := HandleExec(ctx, "echo hello; sleep 30 &", nil, "", func(resp Response) {
		switch resp.Type {
		case ResponseTypeStdout:
			stdout += resp.Data
		case ResponseTypeExit:
			exit = resp
		}
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("HandleExec() error = %v", err)
	}

	if elapsed >= time.Second {
		t.Fatalf("HandleExec() took %v, expected background command to return promptly", elapsed)
	}
	if stdout != "hello\n" {
		t.Fatalf("expected stdout %q, got %q", "hello\n", stdout)
	}
	if exit.Type != ResponseTypeExit {
		t.Fatal("expected exit response")
	}
	if exit.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exit.ExitCode)
	}
}

// shellQuote wraps s in single quotes for /bin/sh, escaping any it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runCatFile writes content to a file and returns what HandleExec streams
// while it runs cat on that file, so the test controls the exact bytes.
func runCatFile(t *testing.T, content string, redirect string) (stdout string, stderr string, exit Response) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := HandleExec(ctx, "cat "+shellQuote(path)+redirect, nil, "", func(resp Response) {
		switch resp.Type {
		case ResponseTypeStdout:
			stdout += resp.Data
		case ResponseTypeStderr:
			stderr += resp.Data
		case ResponseTypeExit:
			exit = resp
		}
	})
	if err != nil {
		t.Fatalf("HandleExec() error = %v", err)
	}
	return stdout, stderr, exit
}

func TestHandleExecStreamsStdoutLineLongerThan64KiB(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("x", 200_000)
	stdout, _, exit := runCatFile(t, line+"\nafter\n", "")

	if stdout != line+"\nafter\n" {
		t.Fatalf("expected %d bytes of stdout, got %d", len(line)+7, len(stdout))
	}
	if exit.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exit.ExitCode)
	}
}

func TestHandleExecStreamsStderrLineLongerThan64KiB(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("x", 200_000)
	_, stderr, _ := runCatFile(t, line+"\n", " >&2")

	if stderr != line+"\n" {
		t.Fatalf("expected %d bytes of stderr, got %d", len(line)+1, len(stderr))
	}
}

func TestHandleExecEndsUnterminatedLineOnChunkBoundaryWithNewline(t *testing.T) {
	t.Parallel()

	// No trailing newline, and the line fills the read buffer exactly.
	line := strings.Repeat("x", 64*1024)
	stdout, _, _ := runCatFile(t, line, "")

	if stdout != line+"\n" {
		t.Fatalf("expected %d bytes ending in a newline, got %d", len(line)+1, len(stdout))
	}
}
