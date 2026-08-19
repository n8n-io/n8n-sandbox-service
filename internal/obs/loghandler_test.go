package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// traceLogger returns a logger writing JSON through TraceHandler, plus a reader
// for the single event it recorded.
func traceLogger(t *testing.T) (*slog.Logger, func() map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(TraceHandler(slog.NewJSONHandler(&buf, nil)))

	return logger, func() map[string]any {
		var event map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &event); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}
		return event
	}
}

func TestTraceHandlerAddsTraceID(t *testing.T) {
	logger, event := traceLogger(t)

	ctx := WithTraceparent(context.Background(), validTraceparent)
	logger.ErrorContext(ctx, "create sandbox failed", "sandbox_id", "abc")

	got := event()
	if got["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v, want the trace id of the context's traceparent", got["trace_id"])
	}
	if got["sandbox_id"] != "abc" {
		t.Fatalf("sandbox_id = %v, want the caller's attribute to survive", got["sandbox_id"])
	}
}

func TestTraceHandlerLeavesUntracedRecordsAlone(t *testing.T) {
	logger, event := traceLogger(t)

	logger.Info("idle sweep failed")

	if got, ok := event()["trace_id"]; ok {
		t.Fatalf("trace_id = %v, want no trace_id on a record logged without a trace", got)
	}
}

// The embedded handler's WithAttrs returns the inner handler, so a logger
// derived with With must keep the wrapper to stay traced.
func TestTraceHandlerSurvivesWith(t *testing.T) {
	logger, event := traceLogger(t)

	ctx := WithTraceparent(context.Background(), validTraceparent)
	logger.With("runner_id", "runner-1").InfoContext(ctx, "create sandbox: runner selected")

	got := event()
	if got["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v, want the derived logger to stay traced", got["trace_id"])
	}
	if got["runner_id"] != "runner-1" {
		t.Fatalf("runner_id = %v, want the derived logger's attribute", got["runner_id"])
	}
}
