package volume

// Kind is the format of a volume object: an ext4 image for Firecracker
// runners, a tar for Docker runners.
type Kind string

const (
	KindExt4 Kind = "ext4"
	KindTar  Kind = "tar"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	return k == KindExt4 || k == KindTar
}
