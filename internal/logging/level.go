package logging

import (
	"fmt"
	"log/slog"
)

// ParseLevel converts a string to slog.Level.
// Accepts "debug", "info", "warn", "error" (case-insensitive).
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log level %q: must be one of debug, info, warn, error", s)
	}
	return level, nil
}
