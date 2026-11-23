// Package logging holds slog helpers shared by the binaries.
package logging

import (
	"errors"
	"fmt"
	"log/slog"
)

var errInvalidLevel = errors.New("invalid log level")

// ParseLevel converts a string to slog.Level.
// Accepts "debug", "info", "warn", "error" (case-insensitive).
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("%w %q: must be one of debug, info, warn, error", errInvalidLevel, s)
	}
	return level, nil
}
