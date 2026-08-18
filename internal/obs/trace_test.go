package obs

import (
	"context"
	"strings"
	"testing"
)

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestTraceIDOf(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"valid", validTraceparent, "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"unsampled flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"future version with extra fields", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra-more", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"empty", "", ""},
		{"too few fields", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", ""},
		{"version 00 with extra field", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", ""},
		{"forbidden version", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ""},
		{"all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"all-zero span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", ""},
		{"uppercase hex", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", ""},
		{"short trace id", "00-4bf92f3577b34da6-00f067aa0ba902b7-01", ""},
		{"not hex", "00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraceIDOf(tc.header); got != tc.want {
				t.Fatalf("TraceIDOf(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestEnsureTraceparentKeepsValidHeader(t *testing.T) {
	if got := EnsureTraceparent(validTraceparent); got != validTraceparent {
		t.Fatalf("EnsureTraceparent() = %q, want the inbound header unchanged", got)
	}
}

func TestEnsureTraceparentDropsTrailingFields(t *testing.T) {
	// A later-version header carries fields we do not understand. We adopt its
	// ids but must not relay the rest: the header is attacker-controlled and we
	// forward the result to the runner over HTTP and gRPC.
	padding := strings.Repeat("a", 4096)
	got := EnsureTraceparent("01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-" + padding)

	if got != validTraceparent {
		t.Fatalf("EnsureTraceparent() = %q, want the canonical form %q", got, validTraceparent)
	}
	if strings.Contains(got, padding) {
		t.Fatal("EnsureTraceparent() relayed caller-supplied trailing fields")
	}
}

func TestEnsureTraceparentIsBounded(t *testing.T) {
	for _, header := range []string{
		"",
		strings.Repeat("-", 100000),
		strings.Repeat("00-4bf92f3577b34da6a3ce929d0e0e4736", 1000),
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-" + strings.Repeat("x", 100000),
	} {
		if got := EnsureTraceparent(header); len(got) != 55 {
			t.Fatalf("EnsureTraceparent(%.20q...) length = %d, want 55", header, len(got))
		}
	}
}

func TestEnsureTraceparentReplacesUnusableHeader(t *testing.T) {
	// The header is attacker-controlled on public endpoints, so anything that
	// is not a valid traceparent must be replaced rather than propagated.
	for _, header := range []string{"", "garbage", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"} {
		got := EnsureTraceparent(header)
		if got == header {
			t.Fatalf("EnsureTraceparent(%q) propagated the unusable header", header)
		}
		if TraceIDOf(got) == "" {
			t.Fatalf("EnsureTraceparent(%q) = %q, which is not a valid traceparent", header, got)
		}
	}
}

func TestNewTraceparentIsUnique(t *testing.T) {
	first, second := NewTraceparent(), NewTraceparent()
	if first == second {
		t.Fatal("NewTraceparent() returned the same value twice")
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithTraceparent(context.Background(), validTraceparent)
	if got := Traceparent(ctx); got != validTraceparent {
		t.Fatalf("Traceparent() = %q, want %q", got, validTraceparent)
	}
	if got := TraceID(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("TraceID() = %q", got)
	}
}

func TestContextWithoutTraceIsEmpty(t *testing.T) {
	if got := Traceparent(context.Background()); got != "" {
		t.Fatalf("Traceparent() = %q, want empty", got)
	}
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("TraceID() = %q, want empty", got)
	}
}
