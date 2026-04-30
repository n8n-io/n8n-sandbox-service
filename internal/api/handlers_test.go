package api

import "testing"

func TestIsValidImageRouteID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"img-deadbeefdeadbeefdeadbeefdeadbeef", true},
		{"sandbox-custom-deadbeefdeadbeefdeadbeefdeadbeef", true},
		{"registry.example/foo:1.2", true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"invalid-id", true},
		{"..", false},
		{"foo..bar", false},
		{string(make([]byte, 513)), false},
	}
	for _, tt := range tests {
		if got := isValidImageRouteID(tt.id); got != tt.want {
			t.Errorf("isValidImageRouteID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
