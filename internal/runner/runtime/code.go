// This file defines the create options of a code sandbox.

package runtime

import (
	"errors"
	"fmt"
)

// ErrCodeSandboxesUnsupported is returned when CreateSandbox refuses a code
// sandbox: the runner has code sandboxes disabled, or its runtime cannot
// start them. Nothing is created.
var ErrCodeSandboxesUnsupported = errors.New("runner does not support code sandboxes")

// ErrCodeImageNotReady is returned when CreateSandbox refuses a code sandbox
// because the code image is not pulled yet. Nothing is created.
var ErrCodeImageNotReady = errors.New("code image not yet available")

// MinCodeMemoryMB is the smallest memory limit of a code sandbox. Docker
// refuses less than 6 MB, and the daemon and runtime need more to start.
const MinCodeMemoryMB = 64

// CodeOptions makes a sandbox a code sandbox: it starts from the code image
// and runs the code daemon.
type CodeOptions struct {
	MemoryMB int64 `json:"memory_mb"`
}

func validateCodeSandboxOptions(options *CodeOptions, egress Egress, volumes Volumes) error {
	if options == nil {
		return nil
	}
	if options.MemoryMB < MinCodeMemoryMB {
		return fmt.Errorf("code_options: memory_mb must be at least %d", MinCodeMemoryMB)
	}
	if egress != EgressNone {
		return errors.New("code_options: a code sandbox must have egress none")
	}
	if len(volumes) > 0 {
		return errors.New("code_options: a code sandbox cannot have volumes")
	}
	return nil
}
