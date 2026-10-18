package codedaemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
)

// rpcResultMaxBytes bounds the body of an RPC result. Results of httpRequest
// can be large. The memory limit of the container is the real bound.
const rpcResultMaxBytes int64 = 1 << 30

// maxOpenCalls bounds the open RPC calls of one task. Each open call can run
// an HTTP request in n8n.
const maxOpenCalls = 64

// callIDRegex is the format of an RPC call id. The worker selects the id, and
// the id goes into the URL of the result request.
var callIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// rpcResult is the answer to one RPC call, from n8n or from the daemon.
type rpcResult struct {
	OK    bool
	Data  json.RawMessage
	Error json.RawMessage
}

type rpcCallback func(rpcResult)

var (
	errCallExists = errors.New("duplicate callId")
	errCallsDone  = errors.New("the task of this call has ended")
	errTooMany    = errors.New("too many open rpc calls")
)

// pendingCalls holds the open RPC calls of one running task.
type pendingCalls struct {
	mu     sync.Mutex
	closed bool
	calls  map[string]rpcCallback
}

func newPendingCalls() *pendingCalls {
	return &pendingCalls{calls: make(map[string]rpcCallback)}
}

// add registers an open call. It fails if the call id is in use or the task
// has ended.
func (p *pendingCalls) add(callID string, cb rpcCallback) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errCallsDone
	}
	if _, dup := p.calls[callID]; dup {
		return errCallExists
	}
	if len(p.calls) >= maxOpenCalls {
		return errTooMany
	}
	p.calls[callID] = cb
	return nil
}

// has reports whether a call is open.
func (p *pendingCalls) has(callID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.calls[callID]
	return ok
}

// take removes an open call and returns its callback, or nil.
func (p *pendingCalls) take(callID string) rpcCallback {
	p.mu.Lock()
	defer p.mu.Unlock()
	cb := p.calls[callID]
	delete(p.calls, callID)
	return cb
}

// close forgets all open calls. Later results get no callback.
func (p *pendingCalls) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.calls = make(map[string]rpcCallback)
}

func errorJSON(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"message": msg})
	return b
}

// rpcResultBody is the body of POST /tasks/{taskId}/rpc/{callId}.
type rpcResultBody struct {
	OK    *bool           `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error json.RawMessage `json:"error,omitempty"`
}

// handleRPCResult gives the result of an RPC call to the worker of the task.
// The call must be open on the running task that the URL names.
func (d *Daemon) handleRPCResult(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskId")
	callID := r.PathValue("callId")
	if !taskIDRegex.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if !callIDRegex.MatchString(callID) {
		writeError(w, http.StatusBadRequest, "invalid call id")
		return
	}
	// Check the task and the call before the body is read: a result that
	// nobody waits for must not cost a large read.
	d.mu.Lock()
	rt := d.running[taskID]
	d.mu.Unlock()
	if rt == nil {
		writeError(w, http.StatusNotFound, "task is not running")
		return
	}
	if !rt.rpc.has(callID) {
		writeError(w, http.StatusNotFound, "rpc call is not open")
		return
	}

	var body rpcResultBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, rpcResultMaxBytes)).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "rpc result too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid rpc result: "+err.Error())
		return
	}
	if body.OK == nil {
		writeError(w, http.StatusBadRequest, "invalid rpc result: missing ok")
		return
	}

	cb := rt.rpc.take(callID)
	if cb == nil {
		writeError(w, http.StatusNotFound, "rpc call is not open")
		return
	}
	cb(rpcResult{OK: *body.OK, Data: body.Data, Error: body.Error})
	w.WriteHeader(http.StatusNoContent)
}
