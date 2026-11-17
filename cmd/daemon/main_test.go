package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const runMainEnv = "SANDBOX_DAEMON_TEST_RUN_MAIN"

// A sandbox user can start the daemon binary with build parameters. It must
// then end with an error: no build, and no second daemon either.
func TestBuildParametersGetOnlyAnErrorOutsideABuilder(t *testing.T) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildParametersGetOnlyAnErrorOutsideABuilder$")
	cmd.Env = append(os.Environ(), runMainEnv+"=1",
		"SANDBOX_VOLUME_BUILD_TAR_BYTES=1024",
		"SANDBOX_VOLUME_BUILD_ENTRIES=1",
		"SANDBOX_VOLUME_BUILD_CONTENT_BYTES=0",
		"SANDBOX_VOLUME_BUILD_DECOMPRESSED_BYTES=1024",
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	// Wait returns this long after the process ends, even if a child of it
	// still holds the output open.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("the daemon ended with %v, want exit status 1\n%s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		// The output is the copying goroutines' until Wait returns.
		<-exited
		t.Fatalf("the daemon kept running\n%s", output.String())
	}
	if !strings.Contains(output.String(), "volume build refused") {
		t.Errorf("no refusal in the output:\n%s", output.String())
	}
	for _, unwanted := range []string{"daemon starting", "daemon listening", "volume build finished"} {
		if strings.Contains(output.String(), unwanted) {
			t.Errorf("the daemon went on to %q:\n%s", unwanted, output.String())
		}
	}
}
