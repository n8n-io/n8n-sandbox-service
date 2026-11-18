// This file loads the runtime command and work directory from daemon configuration.

package codedaemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

func envOr(key, fallback string) string {
	if value := envvar.Get(key); value != "" {
		return value
	}
	return fallback
}

// LoadRuntime reads the trusted image's runtime registry, never the task input.
func (cfg *Config) LoadRuntime() error {
	workDir, err := filepath.Abs(envOr("CODE_DAEMON_WORK_DIR", "/work"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(envOr("CODE_DAEMON_RUNTIMES_FILE", "/etc/code-runtime/runtimes.json"))
	if err != nil {
		return err
	}
	var runtimesByName map[string]struct {
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(data, &runtimesByName); err != nil {
		return fmt.Errorf("decode runtime registry: %w", err)
	}
	name := envOr("CODE_DAEMON_RUNTIME", "n8n-js")
	command := runtimesByName[name].Command
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("missing command for runtime %q", name)
	}
	cfg.WorkDir, cfg.RuntimeCommand = workDir, command
	return nil
}
