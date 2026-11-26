// This file defines the errors of publishing a shared volume.

package volumes

import (
	"errors"
	"fmt"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// Errors of the shared volume operations. The messages of ErrTooLarge and
// ErrInvalidTar are fixed or built from limits only, never from a tar, so they
// may go to a caller. ErrUpload and ErrInterrupted also wrap the error that
// caused them, which can name an address: a caller gets only their own text.
var (
	ErrNotFound          = errors.New("shared volume not found")
	ErrInvalidName       = fmt.Errorf("invalid shared volume name: 1 to %d bytes of a-z, 0-9, '.', '_' and '-', starting and ending with a letter or digit", MaxNameBytes)
	ErrPublishInProgress = errors.New("this shared volume is being published; try again later")
	ErrNameTaken         = errors.New("the name belongs to another shared volume")
	ErrNameMismatch      = errors.New("this shared volume has another name; delete it to publish it under a new one")
	ErrVolumeLimit       = fmt.Errorf("there are already %d shared volumes; delete some first", MaxSharedVolumes)
	ErrPublishLimit      = fmt.Errorf("%d publishes are already in progress on this API instance", MaxConcurrentPublishesPerPod)
	ErrTooLarge          = errors.New("too large")
	ErrInvalidTar        = errors.New("invalid tar")
	ErrDigestMismatch    = errors.New("the upload does not have the sha256 its URL names")
	// ErrInterrupted is a publish that lost its lease or was stopped by a
	// shutdown. Its cleanup removes what it stored, unless that fails or the
	// volume became ready after all.
	ErrInterrupted = errors.New("the publish was interrupted; try again")
	// ErrUpload wraps an error reading the request body: a client that went
	// away, or a stall.
	ErrUpload = errors.New("reading the upload failed")
	// ErrNoCapacity is a publish whose image no runner was free to build in
	// time. Nothing of it is kept.
	ErrNoCapacity = errors.New("no runner was free to build the volume's image; try again later")
	// ErrBuildFailed is the error that every *ImageBuildError wraps.
	ErrBuildFailed = errors.New("the volume's image build failed")
)

// ImageBuildError is an image build that failed on a runner. Reason is one of
// runnerruntime.VolumeBuildReasons, or "transport" when the call failed
// without a reason from the runner, or the runner sent no valid image. Only
// Error carries the cause. Use it in logs only.
type ImageBuildError struct {
	RunnerID string
	Reason   string
	cause    error
}

func (e *ImageBuildError) Error() string {
	return fmt.Sprintf("the volume's image build on runner %s failed (%s): %v", e.RunnerID, e.Reason, e.cause)
}

func (e *ImageBuildError) Unwrap() error { return ErrBuildFailed }

// CallerFault reports whether the tar made the build fail, not the runner.
func (e *ImageBuildError) CallerFault() bool {
	return (&runnerruntime.VolumeBuildError{Reason: e.Reason}).CallerFault()
}

var (
	errTarTooLarge = fmt.Errorf("%w: the tar is over a limit: at most %d entries (counting the directories it leaves out), %d bytes of file content and %d bytes decompressed",
		ErrTooLarge, MaxEntries, MaxContentBytes, MaxDecompressedBytes)
	errUploadTooLarge = fmt.Errorf("%w: the tar may have at most %d bytes", ErrTooLarge, MaxUploadBytes)
	errTarRules       = fmt.Errorf("%w: a volume tar holds only directories, regular files and symbolic links, under relative paths without '..' of at most 4095 bytes, and no entry may go through a symbolic link of the tar", ErrInvalidTar)
	errTarEmpty       = fmt.Errorf("%w: the tar holds no entries", ErrInvalidTar)
	errImageTooLarge  = fmt.Errorf("%w: the ext4 image of this tar would be over %d bytes", ErrTooLarge, MaxImageBytes)
	errTooLargeToRun  = fmt.Errorf("%w: this volume would take more than %d bytes on a runner", ErrTooLarge, volume.MaxSandboxVolumeBytes)

	errLeaseLost    = errors.New("the publish lost its lease")
	errShuttingDown = errors.New("the API is shutting down")
)
