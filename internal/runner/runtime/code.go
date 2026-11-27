// This file defines the create options of a code sandbox.

package runtime

import (
	"errors"
	"fmt"

	"github.com/n8n-io/sandbox-service/internal/codetask"
)

// ErrCodeSandboxesUnsupported is returned when CreateSandbox refuses a code
// sandbox: the runner has code sandboxes disabled, or its runtime cannot
// start them. Nothing is created.
var ErrCodeSandboxesUnsupported = errors.New("runner does not support code sandboxes")

// ErrCodeImageNotReady is returned when CreateSandbox refuses a code sandbox
// because the code image is not pulled yet. Nothing is created.
var ErrCodeImageNotReady = errors.New("code image not yet available")

// CodeOptions makes a sandbox a code sandbox: it starts from the code image
// and runs the code daemon.
type CodeOptions struct {
	MemoryMB int64 `json:"memory_mb"`
}

func validateCodeSandboxOptions(options *CodeOptions, egress Egress, volumes Volumes) error {
	if options == nil {
		return nil
	}
	if options.MemoryMB < codetask.MinSandboxMemoryMB {
		return fmt.Errorf("code_options: memory_mb must be at least %d", codetask.MinSandboxMemoryMB)
	}
	if egress != EgressNone {
		return errors.New("code_options: a code sandbox must have egress none")
	}
	if len(volumes) > 0 {
		return errors.New("code_options: a code sandbox cannot have volumes")
	}
	return nil
}
