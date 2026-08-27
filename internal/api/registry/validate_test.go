package registry

import "testing"

func TestIsValidRunnerHTTPBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"https://example.com", true},
		{"HTTPS://localhost:3000", true},
		{"https://example.com/api/v1", true},
		// Plaintext is rejected: TLSClientConfig does not apply to http URLs,
		// so proxying to one would leak the runner API key.
		{"http://runner:8080", false},
		{"HTTP://localhost:3000", false},
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
			if got := IsValidRunnerHTTPBaseURL(tt.in); got != tt.want {
				t.Fatalf("IsValidRunnerHTTPBaseURL(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
