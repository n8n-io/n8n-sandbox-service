package sandboxproxy

import (
	"context"
	"io"
	"net/http"
	"time"
)

// EarlyReply lets a proxy relay an upstream refusal of an HTTP/1 upload while
// the client is still sending it, as the code daemon's task admission does.
// Without it, net/http reads the unread body before it writes the response,
// so the client waits for its whole upload, or the read timeout, to see the
// refusal. It does nothing for HTTP/2, which needs none of this.
type EarlyReply struct {
	w     http.ResponseWriter
	http1 bool
}

// StartEarlyReply lets w answer r before the handler reads all of r's body.
// Call it before anything reads the body.
func StartEarlyReply(w http.ResponseWriter, r *http.Request) *EarlyReply {
	e := &EarlyReply{w: w, http1: r.ProtoMajor == 1}
	if e.http1 {
		_ = http.NewResponseController(w).EnableFullDuplex()
	}
	return e
}

type earlyReplyKey struct{}

// WithEarlyReply returns a copy of ctx that carries e.
func WithEarlyReply(ctx context.Context, e *EarlyReply) context.Context {
	return context.WithValue(ctx, earlyReplyKey{}, e)
}

// EarlyReplyFrom returns the EarlyReply in ctx, or nil when there is none.
func EarlyReplyFrom(ctx context.Context) *EarlyReply {
	e, _ := ctx.Value(earlyReplyKey{}).(*EarlyReply)
	return e
}

// Refuse closes the connection after the response. Call it before the
// response headers are written.
func (e *EarlyReply) Refuse() {
	if e.http1 {
		e.w.Header().Set("Connection", "close")
	}
}

// RefuseResponse calls Refuse, and StopReading when the ReverseProxy closes
// resp.Body. Call it from ModifyResponse.
func (e *EarlyReply) RefuseResponse(resp *http.Response) {
	e.Refuse()
	resp.Body = &stopReadingOnClose{ReadCloser: resp.Body, e: e}
}

// StopReading fails any pending or later read of the request body. Call it
// once the refusal is written.
func (e *EarlyReply) StopReading() {
	if e.http1 {
		_ = http.NewResponseController(e.w).SetReadDeadline(time.Now())
	}
}

type stopReadingOnClose struct {
	io.ReadCloser
	e *EarlyReply
}

func (b *stopReadingOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.e.StopReading()
	return err
}
