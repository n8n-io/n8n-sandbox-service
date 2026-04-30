package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultListenAddr     = ":8080"
	defaultGRPCListenAddr = ":9090"
	defaultMaxFileBytes   = 10 * 1024 * 1024 // 10 MB
)

// APIConfig contains configuration for the public API gateway.
type APIConfig struct {
	// APIKeys is the set of valid API keys for authenticating public requests.
	APIKeys map[string]struct{}

	// ListenAddr is the TCP address the API gateway listens on.
	ListenAddr string

	// GRPCListenAddr is the TCP address for the private runner registration gRPC server.
	GRPCListenAddr string

	// RegistrationToken is the shared secret runners present as Bearer token on the gRPC stream.
	RegistrationToken string

	// MaxFileBytes is the maximum size of file upload requests accepted by API.
	MaxFileBytes int64

	// RunnerAPIKey is the optional API key sent to runner via X-Api-Key.
	RunnerAPIKey string

	// DataDir is the directory for storing API state.
	DataDir string
}

// LoadAPI reads API gateway configuration from environment variables.
func LoadAPI() (*APIConfig, error) {
	cfg := &APIConfig{
		ListenAddr:     defaultListenAddr,
		GRPCListenAddr: defaultGRPCListenAddr,
		MaxFileBytes:   defaultMaxFileBytes,
		DataDir:        "/tmp/sandbox-api",
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

	if v := os.Getenv("SANDBOX_API_GRPC_LISTEN_ADDR"); v != "" {
		cfg.GRPCListenAddr = v
	}

	cfg.RegistrationToken = os.Getenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN")
	if cfg.RegistrationToken == "" {
		return nil, fmt.Errorf("SANDBOX_API_RUNNER_REGISTRATION_TOKEN must be set")
	}

	if v := os.Getenv("SANDBOX_API_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_API_MAX_FILE_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxFileBytes = n
	}

	cfg.RunnerAPIKey = os.Getenv("SANDBOX_API_RUNNER_API_KEY")

	if v := os.Getenv("SANDBOX_API_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	return cfg, nil
}
