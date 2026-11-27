// Package codetask holds the code sandbox limits that the API and the runner
// share.
package codetask

const (
	// MinSandboxMemoryMB is the smallest memory limit of a code sandbox.
	// Docker refuses less than 6 MB, and the daemon and runtime need more to
	// start.
	MinSandboxMemoryMB = 64

	// DefaultMaxInputBytes is the default limit on the total bytes of a
	// task's input files, as in the code daemon.
	DefaultMaxInputBytes int64 = 64 << 20 // 64 MiB

	// MaxInputBytesCeiling is the highest input limit the code daemon accepts.
	MaxInputBytesCeiling int64 = 1 << 30 // 1 GiB

	// BodyOverheadBytes is the room a task request body has beyond its input
	// files, for the spec and the multipart headers.
	BodyOverheadBytes int64 = 1 << 20 // 1 MiB
)

// MaxBodyBytes returns the limit on a task request body whose input files
// total at most maxInputBytes.
func MaxBodyBytes(maxInputBytes int64) int64 {
	return maxInputBytes + BodyOverheadBytes
}
