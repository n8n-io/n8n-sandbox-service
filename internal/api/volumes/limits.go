// Package volumes handles shared volumes: their limits, and the check of a tar
// while it streams into the volume store.
package volumes

import (
	"regexp"

	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// Limits of a shared volume, in docs/limits.md. Runners fetch the tar within
// their fetch timeout, sized for objects up to 5 GB, and the builder holds a
// set of the tar's directories in memory, so the entries stay at a million.
const (
	MaxTarBytes          int64 = 4 << 30 // 4 GiB
	MaxContentBytes      int64 = 4 << 30 // 4 GiB
	MaxDecompressedBytes int64 = 8 << 30 // 8 GiB
	MaxEntries                 = 1_000_000
	// MaxSharedVolumes counts the publishing and ready ones.
	MaxSharedVolumes = 1000
	// MaxPublishesPerPod bounds the CPU and memory publishes take on one API
	// instance: each hashes, decompresses and reads a tar.
	MaxPublishesPerPod = 2
	MaxNameBytes       = 128
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// ValidName reports whether name may name a shared volume: 1 to 128 bytes of
// a-z, 0-9, ".", "_" and "-", starting and ending with a letter or digit. It
// can't look like a digest reference, which has a ":".
func ValidName(name string) bool {
	return len(name) <= MaxNameBytes && namePattern.MatchString(name)
}

func tarPolicy() safetar.Policy {
	return volume.TarPolicy(MaxEntries, MaxContentBytes, MaxDecompressedBytes)
}
