package sandboxproxy

import (
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

// Refuse marks the response as one that ends the upload, so the connection
// closes after it instead of being drained for reuse. Call it before the
// response headers are written.
func (e *EarlyReply) Refuse() {
	if e.http1 {
		e.w.Header().Set("Connection", "close")
	}
}

// RefuseResponse is Refuse for an upstream response that a ReverseProxy
// relays: it also calls StopReading when the proxy closes resp.Body, which it
// does after it has relayed the whole body. Call it from ModifyResponse.
func (e *EarlyReply) RefuseResponse(resp *http.Response) {
	e.Refuse()
	resp.Body = &stopReadingOnClose{ReadCloser: resp.Body, e: e}
}

// StopReading fails any pending or later read of the request body. Call it
// once the refusal is written. A ReverseProxy does not return until the
// transport stops reading the body to send it upstream, and a client that
// holds the upload open would keep it there until the read timeout.
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
