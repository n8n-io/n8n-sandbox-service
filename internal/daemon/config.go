package daemon

import (
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

// ExecConfig holds the execution settings read from SANDBOX_EXEC_* variables.
type ExecConfig struct {
	// MaxEventBytes is the event history kept per execution.
	MaxEventBytes int64
	// RetainPeriod is how long a completed execution is kept.
	RetainPeriod time.Duration
}

// DefaultExecConfig returns the settings used when no variable is set.
func DefaultExecConfig() ExecConfig {
	return ExecConfig{
		MaxEventBytes: defaultMaxEventBytes,
		RetainPeriod:  defaultExecRetainPeriod,
	}
}

// LoadExecConfig reads SANDBOX_EXEC_MAX_EVENT_BYTES and SANDBOX_EXEC_RETAIN.
func LoadExecConfig() (ExecConfig, error) {
	cfg := DefaultExecConfig()
	if err := envvar.PositiveInt("SANDBOX_EXEC_MAX_EVENT_BYTES", &cfg.MaxEventBytes); err != nil {
		return ExecConfig{}, err
	}
	if err := envvar.PositiveDuration("SANDBOX_EXEC_RETAIN", &cfg.RetainPeriod); err != nil {
		return ExecConfig{}, err
	}
	return cfg, nil
}
