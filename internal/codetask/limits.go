// Package codetask holds the code sandbox task limits that the API and the
// runner share.
package codetask

const (
	// DefaultMaxInputBytes is the default limit on the total bytes of a
	// task's input files (64 MiB), as in the code daemon.
	DefaultMaxInputBytes int64 = 64 << 20

	// MaxInputBytesCeiling is the highest input limit the code daemon accepts.
	MaxInputBytesCeiling int64 = 1 << 30

	// BodyOverheadBytes is the room a task request body has beyond its input
	// files, for the spec and the multipart headers.
	BodyOverheadBytes int64 = 1 << 20
)

// MaxBodyBytes returns the limit on a task request body whose input files
// total at most maxInputBytes.
func MaxBodyBytes(maxInputBytes int64) int64 {
	return maxInputBytes + BodyOverheadBytes
}
