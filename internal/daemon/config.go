package daemon

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

// envInt64 reads an int64 from the named environment variable.
// Returns fallback if the variable is unset or cannot be parsed.
func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Warn("invalid env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return n
}

// envDuration reads a time.Duration from the named environment variable
// (e.g. "10m", "30s"). Returns fallback if the variable is unset or cannot be parsed.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return d
}
