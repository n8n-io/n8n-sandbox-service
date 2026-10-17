// Package codedaemon is the daemon inside a code sandbox. It serves the Task API
// routes that the runner proxies: task slots, a clean worker process per task
// (or one shared worker process in reuse mode), staged inputs and outputs, the
// task output stream and the RPC channel.
//
// The daemon does not know the language of the runtime. It reads the worker
// command from a runtime registry file and never reads task.json.
package codedaemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the daemon settings. The runner sets them as environment
// variables when it creates the code sandbox container.
type Config struct {
	ListenAddr   string
	WorkDir      string
	RuntimesFile string
	Runtime      string
	Slots        int
	TaskMemoryMB int
	SpareWorkers bool
	OutputTTL    time.Duration
	// WorkerEnv is the full environment of each worker process.
	WorkerEnv []string
	// ReuseWorkers runs all tasks of the sandbox in one shared worker
	// process. The slots then limit how many tasks run at the same time. The
	// process, its module cache and changes to built-in objects stay between
	// tasks. Its heap limit is Slots × TaskMemoryMB.
	// CODE_DAEMON_REUSE_WORKERS (default false).
	ReuseWorkers bool
	// AllowLocalClients lets clients on loopback and container addresses use
	// the daemon. Only for tests and local development.
	// CODE_DAEMON_ALLOW_LOCAL_CLIENTS (default false).
	AllowLocalClients bool
}

// RuntimeSpec is one entry of the runtime registry file.
type RuntimeSpec struct {
	Version string   `json:"version"`
	Command []string `json:"command"`
}

const (
	defaultListenAddr   = ":8081"
	defaultWorkDir      = "/work"
	defaultRuntimesFile = "/etc/code-runtime/runtimes.json"
	defaultRuntime      = "n8n-js"
	defaultSlots        = 1
	defaultTaskMemoryMB = 512
	defaultOutputTTL    = 5 * time.Minute
	maxSlots            = 32
)

// LoadConfig reads CODE_DAEMON_* environment variables.
func LoadConfig() (Config, error) {
	cfg := Config{
		ListenAddr:   envOr("CODE_DAEMON_LISTEN_ADDR", defaultListenAddr),
		WorkDir:      envOr("CODE_DAEMON_WORK_DIR", defaultWorkDir),
		RuntimesFile: envOr("CODE_DAEMON_RUNTIMES_FILE", defaultRuntimesFile),
		Runtime:      envOr("CODE_DAEMON_RUNTIME", defaultRuntime),
		Slots:        defaultSlots,
		TaskMemoryMB: defaultTaskMemoryMB,
		SpareWorkers: true,
		OutputTTL:    defaultOutputTTL,
	}
	var err error
	if cfg.Slots, err = envInt("CODE_DAEMON_SLOTS", defaultSlots, 1, maxSlots); err != nil {
		return Config{}, err
	}
	if cfg.TaskMemoryMB, err = envInt("CODE_DAEMON_TASK_MEMORY_MB", defaultTaskMemoryMB, 32, 64*1024); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("CODE_DAEMON_SPARE_WORKERS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("CODE_DAEMON_SPARE_WORKERS must be a boolean, got %q", v)
		}
		cfg.SpareWorkers = b
	}
	if v := os.Getenv("CODE_DAEMON_REUSE_WORKERS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("CODE_DAEMON_REUSE_WORKERS must be a boolean, got %q", v)
		}
		cfg.ReuseWorkers = b
	}
	if v := os.Getenv("CODE_DAEMON_ALLOW_LOCAL_CLIENTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("CODE_DAEMON_ALLOW_LOCAL_CLIENTS must be a boolean, got %q", v)
		}
		cfg.AllowLocalClients = b
	}
	if v := os.Getenv("CODE_DAEMON_OUTPUT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("CODE_DAEMON_OUTPUT_TTL must be a positive duration, got %q", v)
		}
		cfg.OutputTTL = d
	}
	home := envOr("HOME", "/home/node")
	path := envOr("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	cfg.WorkerEnv = []string{"PATH=" + path, "HOME=" + home, "NODE_ENV=production"}
	return cfg, nil
}

// LoadRuntime reads the registry file and returns the entry for name.
func LoadRuntime(file, name string) (RuntimeSpec, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return RuntimeSpec{}, fmt.Errorf("read runtime registry: %w", err)
	}
	var reg map[string]RuntimeSpec
	if err := json.Unmarshal(raw, &reg); err != nil {
		return RuntimeSpec{}, fmt.Errorf("decode runtime registry: %w", err)
	}
	spec, ok := reg[name]
	if !ok {
		return RuntimeSpec{}, fmt.Errorf("runtime %q is not in %s", name, file)
	}
	if len(spec.Command) == 0 {
		return RuntimeSpec{}, fmt.Errorf("runtime %q has no command", name)
	}
	return spec, nil
}

// workerCommand replaces the {taskMemoryMb} placeholder in the command.
func (s RuntimeSpec) workerCommand(taskMemoryMB int) []string {
	out := make([]string, len(s.Command))
	for i, a := range s.Command {
		out[i] = strings.ReplaceAll(a, "{taskMemoryMb}", strconv.Itoa(taskMemoryMB))
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def, lo, hi int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer in [%d, %d], got %q", key, lo, hi, v)
	}
	return n, nil
}
