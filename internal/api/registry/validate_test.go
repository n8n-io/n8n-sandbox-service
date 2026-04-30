package registry

import (
	"errors"
	"testing"
)

func TestIsValidRunnerHTTPBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"http://runner:8080", true},
		{"https://example.com", true},
		{"HTTP://localhost:3000", true},
		{"https://example.com/api/v1", true},
		{"", false},
		{"foo", false},
		{"/relative", false},
		{"//only-scheme-relative", false},
		{"ftp://host", false},
		{"http://", false},
		{"https://", false},
		{"mailto:x@y", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := isValidRunnerHTTPBaseURL(tt.in); got != tt.want {
				t.Fatalf("isValidRunnerHTTPBaseURL(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestUpsertRejectsInvalidHTTPBaseURL(t *testing.T) {
	r := New()
	r.Upsert("id", "not-a-url", true, 1, 0)
	_, err := r.PickRoundRobin()
	if !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickRoundRobin: got %v, want %v", err, ErrNoRunners)
	}
}
