package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultListenAddr   = ":8080"
	defaultMaxFileBytes = 10 * 1024 * 1024 // 10 MB
	defaultRunnerURL    = "http://localhost:8081"
)

// APIConfig contains configuration for the public API gateway.
type APIConfig struct {
	// APIKeys is the set of valid API keys for authenticating public requests.
	APIKeys map[string]struct{}

	// ListenAddr is the TCP address the API gateway listens on.
	ListenAddr string

	// MaxFileBytes is the maximum size of file upload requests accepted by API.
	MaxFileBytes int64

	// RunnerURL is the base URL used to forward requests to the runner service.
	RunnerURL string

	// RunnerAPIKey is the optional API key sent to runner via X-Api-Key.
	RunnerAPIKey string

	// DataDir is the directory for storing API state.
	DataDir string
}

// LoadAPI reads API gateway configuration from environment variables.
func LoadAPI() (*APIConfig, error) {
	cfg := &APIConfig{
		ListenAddr:   defaultListenAddr,
		MaxFileBytes: defaultMaxFileBytes,
		RunnerURL:    defaultRunnerURL,
		DataDir:      "/tmp/sandbox-api",
	}

	rawKeys := os.Getenv("SANDBOX_API_KEYS")
	if rawKeys == "" {
		return nil, fmt.Errorf("SANDBOX_API_KEYS must be set")
	}
	cfg.APIKeys = make(map[string]struct{})
	for _, k := range strings.Split(rawKeys, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			cfg.APIKeys[k] = struct{}{}
		}
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("SANDBOX_API_KEYS contains no valid keys")
	}

	if v := os.Getenv("SANDBOX_API_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}

	if v := os.Getenv("SANDBOX_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_MAX_FILE_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxFileBytes = n
	}

	if v := os.Getenv("SANDBOX_RUNNER_URL"); v != "" {
		cfg.RunnerURL = v
	}
	cfg.RunnerAPIKey = os.Getenv("SANDBOX_RUNNER_API_KEY")

	if v := os.Getenv("SANDBOX_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	return cfg, nil
}
