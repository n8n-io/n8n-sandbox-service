package obs

import (
	"context"
	"sync"
)

// Fields collects attributes for the canonical event that a logging middleware
// emits once per request.
//
// The outermost middleware puts a *Fields on the request context; middleware
// and handlers further in add to it through that pointer. They cannot pass
// values back any other way, because each of them runs with a request derived
// from the outer one, whose context the outer middleware never sees.
type Fields struct {
	mu sync.Mutex
	kv []any
}

type fieldsKey struct{}

// WithFields returns a context carrying a fresh Fields, plus the Fields itself
// so the caller can read it back when the request is done.
func WithFields(ctx context.Context) (context.Context, *Fields) {
	f := &Fields{}
	return context.WithValue(ctx, fieldsKey{}, f), f
}

// FieldsFrom returns the Fields carried on ctx, or nil when there is none.
// Add on a nil *Fields is a no-op, so callers never need a nil check.
func FieldsFrom(ctx context.Context) *Fields {
	f, _ := ctx.Value(fieldsKey{}).(*Fields)
	return f
}

// Add records one key/value pair for the request's event.
func (f *Fields) Add(key string, value any) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv = append(f.kv, key, value)
}

// Attrs returns the collected pairs as a slog-style variadic argument list.
func (f *Fields) Attrs() []any {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any(nil), f.kv...)
}
