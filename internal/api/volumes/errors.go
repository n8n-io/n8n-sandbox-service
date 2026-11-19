package volumes

import (
	"errors"
	"fmt"
)

// Errors of the shared volume operations. The messages are fixed or built from
// limits only, never from a tar, so they may go to a caller.
var (
	ErrNotFound          = errors.New("shared volume not found")
	ErrInvalidName       = fmt.Errorf("invalid shared volume name: 1 to %d bytes of a-z, 0-9, '.', '_' and '-', starting and ending with a letter or digit", MaxNameBytes)
	ErrPublishInProgress = errors.New("this shared volume is being published; try again later")
	ErrNameTaken         = errors.New("the name belongs to another shared volume")
	ErrNameMismatch      = errors.New("this shared volume has another name; delete it to publish it under a new one")
	ErrVolumeLimit       = fmt.Errorf("there are already %d shared volumes; delete some first", MaxSharedVolumes)
	ErrPublishLimit      = fmt.Errorf("%d publishes are already in progress on this API instance", MaxPublishesPerPod)
	ErrTooLarge          = errors.New("too large")
	ErrInvalidTar        = errors.New("invalid tar")
	ErrDigestMismatch    = errors.New("the upload does not have the sha256 its URL names")
	// ErrInterrupted is a publish that lost its lease or was stopped by a
	// shutdown. Nothing of it is kept.
	ErrInterrupted = errors.New("the publish was interrupted; try again")
	// ErrUpload wraps an error reading the request body: a client that went
	// away, or a stall.
	ErrUpload = errors.New("reading the upload failed")
)

var (
	errTarTooLarge = fmt.Errorf("%w: the tar is over a limit: at most %d entries (counting the directories it leaves out), %d bytes of file content and %d bytes decompressed",
		ErrTooLarge, MaxEntries, MaxContentBytes, MaxDecompressedBytes)
	errUploadTooLarge = fmt.Errorf("%w: the tar may have at most %d bytes", ErrTooLarge, MaxTarBytes)
	errTarRules       = fmt.Errorf("%w: a volume tar holds only directories, regular files and symbolic links, under relative paths without '..' of at most 4095 bytes, and no entry may go through a symbolic link of the tar", ErrInvalidTar)
	errTarEmpty       = fmt.Errorf("%w: the tar holds no entries", ErrInvalidTar)

	errLeaseLost    = errors.New("the publish lost its lease")
	errShuttingDown = errors.New("the API is shutting down")
)
