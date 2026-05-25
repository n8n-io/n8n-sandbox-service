package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevelValid(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"DEBUG", slog.LevelDebug},
		{"Info", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseLevel(tt.input)
			if err != nil {
				t.Fatalf("ParseLevel(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLevelInvalid(t *testing.T) {
	for _, input := range []string{"", "trace", "fatal", "not-a-level"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseLevel(input); err == nil {
				t.Fatalf("ParseLevel(%q) should have returned an error", input)
			}
		})
	}
}
