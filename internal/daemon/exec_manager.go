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
	defaultMaxEventBytes    = 16 << 20 // 16 MiB per execution
	defaultExecRetainPeriod = 10 * time.Minute
	cleanupInterval         = time.Minute
)

// ExecManager owns executions and their lifecycle.
type ExecManager struct {
	mu         sync.RWMutex
	executions map[string]*Execution

	maxEventBytes    int64
	execRetainPeriod time.Duration

	stop    chan struct{}
	stopped chan struct{}
}

// NewExecManager creates an ExecManager and starts its cleanup goroutine.
// Configuration is read from environment variables:
//   - SANDBOX_EXEC_MAX_EVENT_BYTES: max bytes of event history per execution (default 16 MiB)
//   - SANDBOX_EXEC_RETAIN: duration to retain completed executions (default 10m)
func NewExecManager() *ExecManager {
	em := &ExecManager{
		executions:       make(map[string]*Execution),
		maxEventBytes:    envInt64("SANDBOX_EXEC_MAX_EVENT_BYTES", int64(defaultMaxEventBytes)),
		execRetainPeriod: envDuration("SANDBOX_EXEC_RETAIN", defaultExecRetainPeriod),
		stop:             make(chan struct{}),
		stopped:          make(chan struct{}),
	}
	go em.cleanupLoop()
	return em
}

// Close stops the cleanup goroutine.
func (em *ExecManager) Close() {
	close(em.stop)
	<-em.stopped
}

func (em *ExecManager) cleanupLoop() {
	defer close(em.stopped)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-em.stop:
			return
		case <-ticker.C:
			em.gc()
		}
	}
}

// gc removes completed executions that have exceeded the retain period.
func (em *ExecManager) gc() {
	em.mu.Lock()
	defer em.mu.Unlock()

	now := time.Now()
	for id, ex := range em.executions {
		ex.mu.Lock()
		expired := !ex.completedAt.IsZero() && now.Sub(ex.completedAt) > em.execRetainPeriod
		ex.mu.Unlock()
		if expired {
			delete(em.executions, id)
			slog.Debug("execution expired", "exec_id", id)
		}
	}
}

// GetOrCreate returns an existing execution for the given exec ID, or creates
// a new one and starts the command in a background goroutine. If execID is
// empty the server generates one.
func (em *ExecManager) GetOrCreate(
	execID string,
	command string,
	env []string,
	workdir string,
	timeout time.Duration,
) *Execution {
	em.mu.Lock()
	defer em.mu.Unlock()

	if execID != "" {
		if ex, ok := em.executions[execID]; ok {
			return ex
		}
	} else {
		execID = uuid.New().String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	ex := &Execution{
		ID:            execID,
		maxEventBytes: em.maxEventBytes,
		cancel:        cancel,
		notify:        make(chan struct{}),
	}

	ex.append(newStartedResponse(execID))

	em.executions[execID] = ex

	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("execution panic", "exec_id", execID, "panic", r)
				ex.append(newErrorResponse(fmt.Sprintf("internal error: %v", r)))
			}
		}()
		if err := HandleExec(ctx, command, env, workdir, func(resp Response) {
			ex.append(resp)
		}); err != nil {
			ex.append(newErrorResponse(err.Error()))
		}
	}()

	return ex
}

// Get returns the execution with the given exec ID, or nil.
func (em *ExecManager) Get(execID string) *Execution {
	em.mu.RLock()
	defer em.mu.RUnlock()
	return em.executions[execID]
}

// Cancel cancels the running command for the given execution. Returns false if
// the execution does not exist.
func (em *ExecManager) Cancel(execID string) bool {
	ex := em.Get(execID)
	if ex == nil {
		return false
	}
	ex.cancel()
	return true
}
