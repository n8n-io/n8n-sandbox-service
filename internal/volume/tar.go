package volume

import "github.com/n8n-io/sandbox-service/internal/safetar"

// maxTarPathBytes is the longest path Linux takes in one system call.
const maxTarPathBytes = 4095

// TarPolicy is what a volume's tar may hold: directories, regular files with
// their exec bit, and symbolic links to any target, so unpack it only through
// an os.Root. The caps are the caller's; maxEntries must be positive and also
// counts the directories a tar leaves out, so it bounds the inodes a volume
// needs. maxDecompressedBytes bounds the work a small gzip tar can ask for.
func TarPolicy(maxEntries int, maxBytes, maxDecompressedBytes int64) safetar.Policy {
	return safetar.Policy{
		AllowSymlinks:        true,
		KeepExecBit:          true,
		MaxEntries:           maxEntries,
		MaxBytes:             maxBytes,
		MaxDecompressedBytes: maxDecompressedBytes,
		MaxPathBytes:         maxTarPathBytes,
		// The image is sized from the entry count.
		CountParentDirectories: true,
	}
}
