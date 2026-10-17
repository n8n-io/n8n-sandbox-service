package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// codeDaemonPollInterval is short, because the create time of a code sandbox
// is on the critical path of the first task in a scope.
const codeDaemonPollInterval = 25 * time.Millisecond

// codeImage returns the code sandbox image, or an error if the runner cannot
// create code sandboxes.
func (m *Runtime) codeImage() (string, error) {
	if m.config.CodeImage == "" {
		return "", fmt.Errorf("code sandboxes are not enabled on this runner: SANDBOX_RUNNER_DOCKER_CODE_IMAGE is not set")
	}
	if !m.codeImageReady.Load() {
		return "", fmt.Errorf("code sandbox image not yet available")
	}
	return m.config.CodeImage, nil
}

// ensureCodeImage pulls the code sandbox image with retries.
func (m *Runtime) ensureCodeImage(ctx context.Context) {
	image := m.config.CodeImage
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if err := m.docker.pullImage(ctx, image); err != nil {
			backoff := min(time.Duration(attempt)*5*time.Second, 60*time.Second)
			slog.Warn("code sandbox image pull failed, retrying", "image", image, "attempt", attempt, "retry_in", backoff, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}
		m.codeImageReady.Store(true)
		slog.Info("code sandbox image ready", "image", image)
		return
	}
}

// codeMemoryLimitMB is the container memory limit of a code sandbox.
func codeMemoryLimitMB(opts *runnerruntime.CreateOptions, overheadMB int64) int64 {
	return int64(opts.Slots)*int64(opts.TaskMemoryMB) + overheadMB
}

// codeDaemonEnv is the environment that configures the code daemon.
func codeDaemonEnv(opts *runnerruntime.CreateOptions) []string {
	return []string{
		"HOME=/home/node",
		"CODE_DAEMON_RUNTIME=" + opts.Runtime,
		"CODE_DAEMON_SLOTS=" + strconv.Itoa(opts.Slots),
		"CODE_DAEMON_TASK_MEMORY_MB=" + strconv.Itoa(opts.TaskMemoryMB),
		"CODE_DAEMON_SPARE_WORKERS=" + strconv.FormatBool(opts.SpareWorkers),
		"CODE_DAEMON_REUSE_WORKERS=" + strconv.FormatBool(opts.ReuseWorkers),
	}
}

// waitForCodeDaemon polls the code daemon until /healthz answers 200. The code
// daemon answers 200 only when it can take a task, so no exec probe is needed.
func waitForCodeDaemon(ctx context.Context, baseURL string) error {
	httpClient := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(codeDaemonPollInterval)
	defer ticker.Stop()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for code daemon health at %s/healthz", baseURL)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
			if err != nil {
				return err
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}
