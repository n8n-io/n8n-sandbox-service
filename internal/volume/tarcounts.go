package volume

import (
	"fmt"

	"github.com/n8n-io/sandbox-service/internal/safetar"
)

// Bounds on a volume tar's length and counts that keep sizing arithmetic from
// overflowing, the builder's included. Product limits are the API's and far
// lower.
const (
	MaxTarBytes   = 1 << 40
	MaxTarEntries = 1 << 28
	// MaxTarDecompressedBytes has room for the most content with its headers
	// and padding.
	MaxTarDecompressedBytes = 4 * MaxTarBytes
)

// TarCounts is what the API counted in a volume's tar under TarPolicy, as
// safetar.Stats counts it. Unpacking the tar is held to these counts.
type TarCounts struct {
	// Entries counts every entry and every directory the tar leaves out.
	Entries int64 `json:"entries"`
	// ContentBytes is the sum of the regular files' sizes.
	ContentBytes int64 `json:"content_bytes"`
	// DecompressedBytes is the tar's length once decompressed: its size for
	// a plain tar.
	DecompressedBytes int64 `json:"decompressed_bytes"`
}

func (counts TarCounts) Validate() error {
	if counts.Entries < 1 || counts.Entries > MaxTarEntries {
		return fmt.Errorf("entries must be 1 to %d, got %d", int64(MaxTarEntries), counts.Entries)
	}
	if counts.ContentBytes < 0 || counts.ContentBytes > MaxTarBytes {
		return fmt.Errorf("content bytes must be 0 to %d, got %d", int64(MaxTarBytes), counts.ContentBytes)
	}
	if counts.DecompressedBytes < 1 || counts.DecompressedBytes > MaxTarDecompressedBytes {
		return fmt.Errorf("decompressed bytes must be 1 to %d, got %d", int64(MaxTarDecompressedBytes), counts.DecompressedBytes)
	}
	// The files' content is part of the decompressed tar.
	if counts.ContentBytes > counts.DecompressedBytes {
		return fmt.Errorf("content bytes %d exceed the decompressed bytes %d", counts.ContentBytes, counts.DecompressedBytes)
	}
	return nil
}

// Policy is TarPolicy capped at the counts, which must be valid. Content
// bytes of 0 leave file content capped by the decompressed bytes only: safetar
// takes a cap of 0 as none.
func (counts TarCounts) Policy() safetar.Policy {
	return TarPolicy(int(counts.Entries), counts.ContentBytes, counts.DecompressedBytes)
}

// MaxSandboxVolumeBytes is the room for one sandbox's volumes on its runner,
// each counted by RunnerBytes: half the runner volume cache's default budget.
// A shared volume that alone takes more is refused at publish.
const MaxSandboxVolumeBytes int64 = 10 << 30 // 10 GiB

const (
	// treeBlockBytes is the block size of ext4 and XFS as runners use them.
	treeBlockBytes = 4 << 10 // 4 KiB
	// treeEntryBytes is the most one entry takes besides its content: a block
	// (a file's partly used last block, a directory's first block, a long
	// link target) and an eighth of one for its name in its directory.
	treeEntryBytes = treeBlockBytes + treeBlockBytes/8
)

// TreeRoom is the most a tree unpacked within counts, which must be valid,
// takes on disk. File content is bounded by the decompressed bytes, which the
// unpack always caps, not by the content bytes, which it leaves uncapped when
// they are 0. A 64th more is for the extent blocks of fragmented files, and
// the root is one more entry.
func (counts TarCounts) TreeRoom() (treeBytes, treeInodes int64) {
	content := (counts.DecompressedBytes + treeBlockBytes - 1) / treeBlockBytes * treeBlockBytes
	return content + content/64 + (counts.Entries+1)*treeEntryBytes, counts.Entries + 1
}

// RunnerBytes is the most a runner's volume cache sets aside for an object of
// kind with objectBytes bytes: an image its size; a tar its size and the room
// of its tree, since the tar stays on disk until the tree is complete. counts
// are the tar's and must be valid.
func RunnerBytes(kind Kind, objectBytes int64, counts TarCounts) int64 {
	if kind != KindTar {
		return objectBytes
	}
	treeBytes, _ := counts.TreeRoom()
	return objectBytes + treeBytes
}
