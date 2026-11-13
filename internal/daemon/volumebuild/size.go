package volumebuild

const (
	blockBytes      = 4096
	inodeBytes      = 256
	inodesPerBlock  = blockBytes / inodeBytes
	blocksPerGroup  = 8 * blockBytes // what one bitmap block maps: 128 MiB
	groupDescsBlock = blockBytes / 64

	// Inodes ext4 keeps for itself, lost+found included.
	reservedInodes = 11
	// Inodes and blocks to spare, so a small image also has room for a
	// directory the count missed: safetar tracks directories by hash.
	spareEntries = 256
	// Root and lost+found.
	rootBlocks = 5
	// The most ext4 withholds from writers.
	maxReservedBlocks = 4096
	// The most inode table blocks a group has under this rule: two inodes
	// for every two blocks.
	maxGroupInodeBlocks = blocksPerGroup / inodesPerBlock
)

// The image is ext4 with 4 KiB blocks and 256-byte inodes, without a journal
// or blocks reserved for root. Inodes and ImageBytes size it for a tar of
// entries entries and contentBytes of file content:
//
//   - Entries includes the directories the tar leaves out, which the volume
//     tar policy counts as entries. Every entry gets two inodes and two
//     blocks: one each for the entry itself (a file's partly used last block,
//     a directory's first block, a long link target) and one each to spare.
//   - Names in directories get an eighth of a block per entry: two 255-byte
//     names, or sixteen of 24 bytes in half-full hashed directory blocks.
//   - Content gets its blocks plus 1/64 for the extent blocks of fragmented
//     files.
//   - Inode tables hold 16 inodes per block.
//   - Each 128 MiB block group has two bitmaps, a superblock copy, the group
//     descriptors (64 per block), and an inode table rounded up to a block.
//   - ext4 withholds 2% of the blocks from writers, 4096 at most.
//   - mke2fs leaves off a last group too short for its own tables plus 50
//     blocks, so one group's tables are added once more.
//
// The rule aims high: nothing shrinks the image afterwards.

// Inodes is how many inodes the guest formats the output drive with.
func Inodes(entries int64) int64 {
	return 2*entries + spareEntries + reservedInodes
}

// ImageBytes is the size of the output drive. The params must be valid.
func ImageBytes(params Params) int64 {
	content := divCeil(params.ContentBytes, blockBytes)
	inodeTable := divCeil(Inodes(params.Entries), inodesPerBlock)
	payload := content + content/64 + 1 +
		2*params.Entries + spareEntries +
		divCeil(params.Entries, 8) + rootBlocks + inodeTable

	// The group tables depend on the size they are part of, so grow the
	// group count until the image fits in it. maxBytes and maxEntries keep
	// the descriptors of a group far below a group, so this ends.
	groups := divCeil(payload, blocksPerGroup)
	for {
		descriptors := divCeil(groups, groupDescsBlock)
		blocks := payload + groups*(4+descriptors)
		blocks += min(divCeil(blocks, 49), maxReservedBlocks)
		blocks += min(inodeTable, maxGroupInodeBlocks) + descriptors + 64
		if fits := divCeil(blocks, blocksPerGroup); fits > groups {
			groups = fits
			continue
		}
		return blocks * blockBytes
	}
}

func divCeil(value, divisor int64) int64 {
	return (value + divisor - 1) / divisor
}
