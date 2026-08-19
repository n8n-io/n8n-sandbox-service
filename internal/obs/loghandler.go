package obs

import (
	"context"
	"log/slog"
)

// TraceHandler wraps h so that every record logged with a context carrying a
// traceparent gets a trace_id attribute.
//
// It is what makes the diagnostics a request emits along the way joinable with
// the canonical event for that request: log through the context-taking slog
// functions (InfoContext, ErrorContext, ...) and the join key comes for free.
// Records logged without a context, and those from background work that has no
// trace, are passed through untouched rather than tagged with an empty id.
func TraceHandler(h slog.Handler) slog.Handler {
	return traceHandler{Handler: h}
}

type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if traceID := TraceID(ctx); traceID != "" {
		rec = rec.Clone()
		rec.AddAttrs(slog.String("trace_id", traceID))
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs and WithGroup rewrap the derived handler; the embedded methods
// would return the inner handler and quietly drop the trace id.

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
