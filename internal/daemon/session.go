package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultMaxEventBytes       = 16 << 20 // 16 MiB per session
	defaultSessionRetainPeriod = 10 * time.Minute
	cleanupInterval            = time.Minute
)

// SessionManager owns exec sessions and their lifecycle.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ExecSession

	maxEventBytes       int64
	sessionRetainPeriod time.Duration

	stop    chan struct{}
	stopped chan struct{}
}

// NewSessionManager creates a SessionManager and starts its cleanup goroutine.
// Configuration is read from environment variables:
//   - SANDBOX_EXEC_MAX_EVENT_BYTES: max bytes of event history per session (default 16 MiB)
//   - SANDBOX_EXEC_SESSION_RETAIN: duration to retain completed sessions (default 10m)
func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions:            make(map[string]*ExecSession),
		maxEventBytes:       envInt64("SANDBOX_EXEC_MAX_EVENT_BYTES", int64(defaultMaxEventBytes)),
		sessionRetainPeriod: envDuration("SANDBOX_EXEC_SESSION_RETAIN", defaultSessionRetainPeriod),
		stop:                make(chan struct{}),
		stopped:             make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

// Close stops the cleanup goroutine.
func (sm *SessionManager) Close() {
	close(sm.stop)
	<-sm.stopped
}

func (sm *SessionManager) cleanupLoop() {
	defer close(sm.stopped)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.stop:
			return
		case <-ticker.C:
			sm.gc()
		}
	}
}

// gc removes completed sessions that have exceeded the retain period.
func (sm *SessionManager) gc() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, sess := range sm.sessions {
		sess.mu.Lock()
		expired := !sess.completedAt.IsZero() && now.Sub(sess.completedAt) > sm.sessionRetainPeriod
		sess.mu.Unlock()
		if expired {
			delete(sm.sessions, id)
			slog.Debug("session expired", "exec_id", id)
		}
	}
}

// GetOrCreate returns an existing session for the given exec ID, or creates
// a new one and starts the command in a background goroutine. If execID is
// empty the server generates one.
func (sm *SessionManager) GetOrCreate(
	execID string,
	command string,
	env []string,
	workdir string,
	timeout time.Duration,
) *ExecSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if execID != "" {
		if sess, ok := sm.sessions[execID]; ok {
			return sess
		}
	} else {
		execID = uuid.New().String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	sess := &ExecSession{
		ID:            execID,
		maxEventBytes: sm.maxEventBytes,
		cancel:        cancel,
		notify:        make(chan struct{}),
	}

	sess.append(newSessionResponse(execID))

	sm.sessions[execID] = sess

	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("exec session panic", "exec_id", execID, "panic", r)
				sess.append(newErrorResponse(fmt.Sprintf("internal error: %v", r)))
			}
		}()
		if err := HandleExec(ctx, command, env, workdir, func(resp Response) {
			sess.append(resp)
		}); err != nil {
			sess.append(newErrorResponse(err.Error()))
		}
	}()

	return sess
}

// Get returns the session with the given exec ID, or nil.
func (sm *SessionManager) Get(execID string) *ExecSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[execID]
}

// Cancel cancels the running command for the given session. Returns false if
// the session does not exist.
func (sm *SessionManager) Cancel(execID string) bool {
	sess := sm.Get(execID)
	if sess == nil {
		return false
	}
	sess.cancel()
	return true
}
