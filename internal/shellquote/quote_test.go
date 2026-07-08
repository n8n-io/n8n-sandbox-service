package shellquote

import "testing"

func TestQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "'abc'"},
		{"it's", `'it'"'"'s'`},
	}
	for _, tc := range tests {
		if got := Quote(tc.in); got != tc.want {
			t.Fatalf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
