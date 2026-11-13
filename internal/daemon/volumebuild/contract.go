// Package volumebuild turns a tar into an ext4 volume image inside a builder
// microVM, and holds the contract between that guest and the runner booting it.
//
// The guest sees the builder's drives in attach order:
//
//	/dev/vda  rootfs, read-only
//	/dev/vdb  the tar, read-only, padded with zeros to DriveBytes
//	/dev/vdc  output, read-write: ImageBytes long, becomes the image
//	/dev/vdd  status, read-write, StatusBytes long
//
// The build parameters reach the daemon as environment: the kernel hands init
// every NAME=value pair of its command line that it does not know. The guest
// has no /proc to read the command line from, and a name with a dot never
// reaches init: the kernel takes it for a module parameter.
package volumebuild

import (
	"fmt"
	"strconv"
	"strings"
)

// The builder's drives, in attach order.
const (
	DriveRootfs = iota
	DriveTar
	DriveOutput
	DriveStatus
)

func devicePath(drive int) string {
	return "/dev/vd" + string(rune('a'+drive))
}

const sectorBytes = 512

// DriveBytes is the size of the tar drive for a tar of that length: virtio
// drives hold whole sectors.
func DriveBytes(tarBytes int64) int64 {
	return (tarBytes + sectorBytes - 1) / sectorBytes * sectorBytes
}

const (
	envTarBytes          = "SANDBOX_VOLUME_BUILD_TAR_BYTES"
	envEntries           = "SANDBOX_VOLUME_BUILD_ENTRIES"
	envContentBytes      = "SANDBOX_VOLUME_BUILD_CONTENT_BYTES"
	envDecompressedBytes = "SANDBOX_VOLUME_BUILD_DECOMPRESSED_BYTES"
)

// Bounds on Params that keep the sizing arithmetic from overflowing. Product
// limits are the caller's and far lower.
const (
	maxBytes   = 1 << 40
	maxEntries = 1 << 28
	// Room for the most content with its headers and padding.
	maxDecompressedBytes = 4 * maxBytes
)

// Params is what the runner tells the guest about the tar. Entries,
// ContentBytes and DecompressedBytes are also the most the guest unpacks,
// counted as safetar.Stats counts them.
type Params struct {
	// TarBytes is the tar's length on the tar drive.
	TarBytes int64
	// Entries is the tar's total entry count.
	Entries int64
	// ContentBytes is the sum of its regular file sizes.
	ContentBytes int64
	// DecompressedBytes is the tar's length once decompressed: TarBytes for
	// a plain tar. It bounds what a small gzip tar makes the guest read.
	DecompressedBytes int64
}

func (p Params) Validate() error {
	if p.TarBytes < 1 || p.TarBytes > maxBytes {
		return fmt.Errorf("tar bytes must be 1 to %d, got %d", int64(maxBytes), p.TarBytes)
	}
	if p.Entries < 1 || p.Entries > maxEntries {
		return fmt.Errorf("entries must be 1 to %d, got %d", int64(maxEntries), p.Entries)
	}
	if p.ContentBytes < 0 || p.ContentBytes > maxBytes {
		return fmt.Errorf("content bytes must be 0 to %d, got %d", int64(maxBytes), p.ContentBytes)
	}
	if p.DecompressedBytes < 1 || p.DecompressedBytes > maxDecompressedBytes {
		return fmt.Errorf("decompressed bytes must be 1 to %d, got %d", int64(maxDecompressedBytes), p.DecompressedBytes)
	}
	return nil
}

// KernelArgs is what the runner appends to the builder's kernel command line.
func (p Params) KernelArgs() string {
	return fmt.Sprintf("%s=%d %s=%d %s=%d %s=%d", envTarBytes, p.TarBytes, envEntries, p.Entries,
		envContentBytes, p.ContentBytes, envDecompressedBytes, p.DecompressedBytes)
}

// ParamsFromEnv reads the parameters KernelArgs wrote. Build mode is requested
// if any of them is set; all of them must then be set and valid.
func ParamsFromEnv(lookup func(name string) (string, bool)) (params Params, requested bool, err error) {
	var problems []string
	for _, field := range []struct {
		name string
		dest *int64
	}{
		{envTarBytes, &params.TarBytes},
		{envEntries, &params.Entries},
		{envContentBytes, &params.ContentBytes},
		{envDecompressedBytes, &params.DecompressedBytes},
	} {
		raw, ok := lookup(field.name)
		if !ok {
			problems = append(problems, field.name+" is not set")
			continue
		}
		requested = true
		value, parseErr := parseDecimal(raw)
		if parseErr != nil {
			problems = append(problems, field.name+" "+parseErr.Error())
			continue
		}
		*field.dest = value
	}
	if !requested {
		return Params{}, false, nil
	}
	if len(problems) > 0 {
		return Params{}, true, fmt.Errorf("volume build parameters: %s", strings.Join(problems, "; "))
	}
	if err := params.Validate(); err != nil {
		return Params{}, true, fmt.Errorf("volume build parameters: %w", err)
	}
	return params, true, nil
}

// parseDecimal accepts only what KernelArgs writes: digits, no sign or space.
func parseDecimal(raw string) (int64, error) {
	if raw == "" || len(raw) > 19 || strings.Trim(raw, "0123456789") != "" {
		return 0, fmt.Errorf("must be a decimal number")
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a decimal number")
	}
	return value, nil
}

// Why a build failed, as the status record reports it.
const (
	// ReasonTar is an invalid archive, a refused entry, or a tar that is not
	// the one the parameters describe.
	ReasonTar = "tar"
	// ReasonTooLarge is more entries, content or decompressed bytes than the
	// parameters allow.
	ReasonTooLarge = "too_large"
	// ReasonNoSpace is an output drive out of blocks or inodes.
	ReasonNoSpace  = "no_space"
	ReasonFormat   = "format"
	ReasonMount    = "mount"
	ReasonFsck     = "fsck"
	ReasonInternal = "internal"
)

func knownReason(reason string) bool {
	switch reason {
	case ReasonTar, ReasonTooLarge, ReasonNoSpace, ReasonFormat, ReasonMount, ReasonFsck, ReasonInternal:
		return true
	}
	return false
}
