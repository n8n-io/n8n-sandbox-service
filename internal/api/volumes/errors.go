// This file defines the errors of publishing a shared volume.

package volumes

import (
	"errors"
	"fmt"
)

// Errors of a tar upload. The messages are fixed or built from limits only,
// never from a tar, so they may go to a caller.
var (
	ErrTooLarge   = errors.New("too large")
	ErrInvalidTar = errors.New("invalid tar")
	// ErrUpload wraps an error reading the request body: a client that went
	// away, or a stall.
	ErrUpload = errors.New("reading the upload failed")
)

var (
	errTarTooLarge = fmt.Errorf("%w: the tar is over a limit: at most %d entries (counting the directories it leaves out), %d bytes of file content and %d bytes decompressed",
		ErrTooLarge, MaxEntries, MaxContentBytes, MaxDecompressedBytes)
	errTarRules = fmt.Errorf("%w: a volume tar holds only directories, regular files and symbolic links, under relative paths without '..' of at most 4095 bytes, and no entry may go through a symbolic link of the tar", ErrInvalidTar)
	errTarEmpty = fmt.Errorf("%w: the tar holds no entries", ErrInvalidTar)
)
