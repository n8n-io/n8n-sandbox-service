package codedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// channelFrame is one JSON text frame on the RPC channel.
type channelFrame struct {
	Type           string          `json:"type"`
	RuntimeVersion string          `json:"runtimeVersion,omitempty"`
	TaskID         string          `json:"taskId,omitempty"`
	CallID         string          `json:"callId,omitempty"`
	Name           string          `json:"name,omitempty"`
	Params         json.RawMessage `json:"params,omitempty"`
	OK             *bool           `json:"ok,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
	Error          json.RawMessage `json:"error,omitempty"`
}

// rpcResult is the answer to one RPC call, from n8n or from the daemon.
type rpcResult struct {
	OK    bool
	Data  json.RawMessage
	Error json.RawMessage
}

type rpcCallback func(rpcResult)

// channel is the one RPC channel WebSocket of the sandbox.
type channel struct {
	conn *websocket.Conn

	mu      sync.Mutex
	closed  bool
	pending map[string]rpcCallback // key: taskId + "\x00" + callId
}

func pendingKey(taskID, callID string) string { return taskID + "\x00" + callID }

func newChannel(conn *websocket.Conn) *channel {
	return &channel{conn: conn, pending: make(map[string]rpcCallback)}
}

func errorJSON(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"message": msg})
	return b
}

// call registers an RPC call and sends the rpc:call frame. cb runs when the
// result arrives, or when the channel closes. The registration is synchronous,
// so a dropTask that comes after it removes the call. The frame is written in
// the background, and only if the call is still open then.
func (c *channel) call(ctx context.Context, taskID, callID, name string, params json.RawMessage, cb rpcCallback) {
	key := pendingKey(taskID, callID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		go cb(rpcResult{OK: false, Error: errorJSON("RPC channel is closed")})
		return
	}
	if _, dup := c.pending[key]; dup {
		c.mu.Unlock()
		go cb(rpcResult{OK: false, Error: errorJSON("duplicate callId")})
		return
	}
	c.pending[key] = cb
	c.mu.Unlock()

	go func() {
		c.mu.Lock()
		_, open := c.pending[key]
		c.mu.Unlock()
		if !open {
			// The task ended before the write.
			return
		}
		frame := channelFrame{Type: "rpc:call", TaskID: taskID, CallID: callID, Name: name, Params: params}
		if err := c.write(ctx, frame); err != nil {
			if cb := c.take(key); cb != nil {
				cb(rpcResult{OK: false, Error: errorJSON("RPC channel write failed: " + err.Error())})
			}
		}
	}()
}

func (c *channel) write(ctx context.Context, frame channelFrame) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, b)
}

func (c *channel) take(key string) rpcCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	cb := c.pending[key]
	delete(c.pending, key)
	return cb
}

// dropTask forgets the open calls of a task that has ended.
func (c *channel) dropTask(taskID string) {
	prefix := taskID + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.pending {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(c.pending, k)
		}
	}
}

// failAll closes the channel for new calls and fails all open calls.
func (c *channel) failAll(reason string) {
	c.mu.Lock()
	c.closed = true
	cbs := make([]rpcCallback, 0, len(c.pending))
	for k, cb := range c.pending {
		cbs = append(cbs, cb)
		delete(c.pending, k)
	}
	c.mu.Unlock()
	// Async: a callback writes to a worker, which must not block the channel.
	for _, cb := range cbs {
		go cb(rpcResult{OK: false, Error: errorJSON(reason)})
	}
}

// readLoop handles frames from n8n until the connection closes.
func (c *channel) readLoop(ctx context.Context) error {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var f channelFrame
		if err := json.Unmarshal(data, &f); err != nil {
			slog.Warn("channel: invalid frame", "err", err)
			continue
		}
		switch f.Type {
		case "rpc:result":
			cb := c.take(pendingKey(f.TaskID, f.CallID))
			if cb == nil {
				// The task ended, or the frame names a call that does not exist.
				continue
			}
			res := rpcResult{OK: f.OK != nil && *f.OK, Data: f.Data, Error: f.Error}
			go cb(res)
		default:
			slog.Warn("channel: unknown frame type", "type", f.Type)
		}
	}
}

var errChannelBusy = errors.New("channel already open")
