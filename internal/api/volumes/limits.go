// This file defines the limits of a shared volume and the rule for its name.

// Package volumes publishes, lists and removes shared volumes: the steps
// between a publish request, the shared_volumes rows and the volume store.
package volumes

import "regexp"

// Limits of a shared volume.
const (
	// MaxUploadBytes is the tar as uploaded, plain or gzip. Runners fetch it
	// within SANDBOX_RUNNER_VOLUME_FETCH_TIMEOUT, 5 minutes by default.
	MaxUploadBytes int64 = 4 << 30 // 4 GiB
	// MaxContentBytes is the same size: a plain tar holds no more content than
	// its own size.
	MaxContentBytes int64 = 4 << 30 // 4 GiB
	// MaxDecompressedBytes bounds the work of the API and the builder on a
	// small gzip.
	MaxDecompressedBytes int64 = 8 << 30 // 8 GiB
	// MaxEntries counts the directories a tar leaves out too. The builder keeps
	// them in memory, about 50 bytes each, in a guest with 512 MiB.
	MaxEntries = 1_000_000
	// MaxSharedVolumes counts the publishing and ready ones. The publisher
	// deletes old versions, so only a faulty publisher reaches the limit.
	MaxSharedVolumes = 1000
	// MaxConcurrentPublishesPerPod bounds the CPU and memory publishes take on
	// one API instance: each uses about one core and 50 MB to hash, decompress
	// and read a tar.
	MaxConcurrentPublishesPerPod = 2
	// MaxNameBytes fits a name such as n8n-assistant-2.46.0-beta.1.
	MaxNameBytes = 128
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// ValidName reports whether name may name a shared volume: 1 to 128 bytes of
// a-z, 0-9, ".", "_" and "-", starting and ending with a letter or digit. It
// can't look like a digest reference, which has a ":".
func ValidName(name string) bool {
	return len(name) <= MaxNameBytes && namePattern.MatchString(name)
}
