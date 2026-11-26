// This file loads the daemon configuration, the runtime command, and the
// task limits and timeouts, and sets their defaults.

package codedaemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

const (
	defaultMaxTaskTimeout = time.Minute
	// Hard cap on the configured maximum. It matches the runner's default exec
	// limit and stays below the API's default response activity window (30m),
	// so a running task keeps its sandbox from going idle.
	maxTaskTimeoutCap = 15 * time.Minute
)

var errMissingCommand = errors.New("missing command for runtime")

type Config struct {
	ListenAddr string
	// AllowLocalClients disables the check that rejects requests from inside
	// the sandbox. Only for debugging and local development.
	AllowLocalClients bool
	MaxConcurrency    int // Number of lanes. Each running or uploading task holds one.

	WorkDir        string   // root for task directories and the runtime's working directory
	RuntimeCommand []string // executable followed by its arguments, from the runtime registry
	Limits         Limits   // per-task budgets; non-positive values use defaults
	// RuntimeLogWrites caps the writes to each of the runtime's stdout and
	// stderr that reach the daemon log. Non-positive uses the default of 16.
	RuntimeLogWrites int
	MaxTimeout       time.Duration // Service cap on the execution budget a request sets.
}

func LoadConfig() (Config, error) {
	cfg := Config{ListenAddr: ":8081", MaxConcurrency: 1, RuntimeLogWrites: defaultRuntimeLogWrites}
	envvar.String("CODE_DAEMON_LISTEN_ADDR", &cfg.ListenAddr)
	if err := envvar.Bool("CODE_DAEMON_ALLOW_LOCAL_CLIENTS", &cfg.AllowLocalClients); err != nil {
		return Config{}, err
	}
	if err := envvar.IntBetween("CODE_DAEMON_MAX_CONCURRENCY", &cfg.MaxConcurrency, 1, 32); err != nil {
		return Config{}, err
	}
	limits, err := loadLimits()
	if err != nil {
		return Config{}, err
	}
	cfg.Limits = limits
	if err := envvar.IntBetween("CODE_DAEMON_RUNTIME_LOG_WRITES", &cfg.RuntimeLogWrites, 1, 10_000); err != nil {
		return Config{}, err
	}
	if cfg.MaxTimeout, err = loadMaxTaskTimeout(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

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
		return fmt.Errorf("%w %q", errMissingCommand, name)
	}
	cfg.WorkDir, cfg.RuntimeCommand = workDir, command
	return nil
}

// Limits caps aggregate input and output bytes and file counts in each direction.
type Limits struct {
	InputBytes, OutputBytes, Files int
}

// withDefaults fills in limits that were not set to a positive value.
func (l Limits) withDefaults() Limits {
	if l.InputBytes <= 0 {
		l.InputBytes = 64 << 20 // 64 MiB
	}
	if l.OutputBytes <= 0 {
		l.OutputBytes = 64 << 20 // 64 MiB
	}
	if l.Files <= 0 {
		l.Files = 256
	}
	return l
}

// loadLimits validates environment overrides and uses defaults for unset limits.
func loadLimits() (Limits, error) {
	l := (Limits{}).withDefaults()
	for _, item := range []struct {
		key   string
		value *int
		max   int
	}{
		{"CODE_DAEMON_MAX_INPUT_BYTES", &l.InputBytes, 1 << 30},   // 1 GiB
		{"CODE_DAEMON_MAX_OUTPUT_BYTES", &l.OutputBytes, 1 << 30}, // 1 GiB
		{"CODE_DAEMON_MAX_FILES", &l.Files, 4096},
	} {
		if err := envvar.IntBetween(item.key, item.value, 1, item.max); err != nil {
			return Limits{}, err
		}
	}
	return l, nil
}

// loadMaxTaskTimeout returns the service cap on the execution duration.
func loadMaxTaskTimeout() (time.Duration, error) {
	value := defaultMaxTaskTimeout
	if err := envvar.DurationBetween("CODE_DAEMON_MAX_TASK_TIMEOUT", &value, time.Millisecond, maxTaskTimeoutCap); err != nil {
		return 0, err
	}
	return value, nil
}

// taskTimeout uses the requested duration, but never more than the service cap.
func (cfg Config) taskTimeout(requestMillis int64) time.Duration {
	limit := cfg.MaxTimeout
	// Compare before converting, since the request value may overflow a duration.
	if requestMillis > 0 && requestMillis <= limit.Milliseconds() {
		return time.Duration(requestMillis) * time.Millisecond
	}
	return limit
}
