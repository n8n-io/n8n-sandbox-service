package daemon

import (
	"context"
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
