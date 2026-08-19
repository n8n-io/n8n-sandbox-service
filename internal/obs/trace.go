// Package obs carries a W3C trace context across the API and the runner, and
// collects the fields that make up a request's canonical log event.
//
// Nothing here depends on OpenTelemetry. The ids are W3C-shaped so that events
// carrying them can become spans later without re-instrumenting.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// HeaderTraceparent is the W3C header the API and the runner exchange, both as
// an HTTP header and as gRPC metadata.
const HeaderTraceparent = "traceparent"

const (
	zeroTraceID = "00000000000000000000000000000000"
	zeroSpanID  = "0000000000000000"
)

type traceparentKey struct{}

// WithTraceparent returns a context carrying tp.
func WithTraceparent(ctx context.Context, tp string) context.Context {
	return context.WithValue(ctx, traceparentKey{}, tp)
}

// Traceparent returns the traceparent carried on ctx, or "" when there is none.
// Use it to forward the trace context to the next hop.
func Traceparent(ctx context.Context) string {
	tp, _ := ctx.Value(traceparentKey{}).(string)
	return tp
}

// TraceID returns just the trace-id field of the traceparent on ctx, which is
// what log events carry so that events from both processes can be joined.
func TraceID(ctx context.Context) string {
	return TraceIDOf(Traceparent(ctx))
}

// EnsureTraceparent returns the traceparent to use for a request: the inbound
// header's ids in canonical form when they are usable, and a freshly minted
// traceparent otherwise.
//
// The header is attacker-controlled on public endpoints and we forward it to
// the runner, so the result is always rebuilt from the four fields we parsed.
// A caller cannot make us relay bytes of their choosing: what we send on is at
// most 55 characters of hex drawn from fields we validated.
func EnsureTraceparent(header string) string {
	traceID, spanID, flags, ok := parseTraceparent(header)
	if !ok {
		return NewTraceparent()
	}
	return "00-" + traceID + "-" + spanID + "-" + flags
}

// NewTraceparent mints a sampled traceparent with random trace and span ids.
func NewTraceparent() string {
	return "00-" + randomHex(16) + "-" + randomHex(8) + "-01"
}

// TraceIDOf returns the trace-id field of a traceparent header value, or ""
// when the value is not one we adopt.
func TraceIDOf(tp string) string {
	traceID, _, _, _ := parseTraceparent(tp)
	return traceID
}

// parseTraceparent splits a traceparent into the fields we understand. SplitN
// caps the slice at five regardless of how long the header is, so an oversized
// header costs nothing to reject.
func parseTraceparent(tp string) (traceID, spanID, flags string, ok bool) {
	parts := strings.SplitN(tp, "-", 5)
	if len(parts) < 4 {
		return "", "", "", false
	}
	version := parts[0]
	traceID, spanID, flags = parts[1], parts[2], parts[3]
	// Version ff is forbidden by the spec. Version 00 has exactly four fields;
	// later versions may append more, which we drop rather than relay.
	if !isHex(version, 2) || version == "ff" {
		return "", "", "", false
	}
	if version == "00" && len(parts) != 4 {
		return "", "", "", false
	}
	if !isHex(traceID, 32) || traceID == zeroTraceID {
		return "", "", "", false
	}
	if !isHex(spanID, 16) || spanID == zeroSpanID {
		return "", "", "", false
	}
	if !isHex(flags, 2) {
		return "", "", "", false
	}
	return traceID, spanID, flags, true
}

// isHex reports whether s is exactly n lowercase hex digits, which is what the
// traceparent spec allows.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	// crypto/rand.Read cannot fail: it crashes the program if the system
	// source is unavailable.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
