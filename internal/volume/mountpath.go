package volume

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

const maxMountPathBytes = 255

var mountRoots = []string{"/mnt/", "/home/user/"}

// ValidateMountPath checks where a volume may be mounted: an absolute, clean
// path of at most 255 bytes without control characters, below /mnt/ or
// /home/user/.
func ValidateMountPath(mountPath string) error {
	if len(mountPath) > maxMountPathBytes {
		return fmt.Errorf("path is longer than %d bytes", maxMountPathBytes)
	}
	if !strings.HasPrefix(mountPath, "/") || path.Clean(mountPath) != mountPath {
		return fmt.Errorf("path %q is not absolute and clean", mountPath)
	}
	if strings.ContainsFunc(mountPath, unicode.IsControl) {
		return fmt.Errorf("path %q contains a control character", mountPath)
	}
	for _, root := range mountRoots {
		if strings.HasPrefix(mountPath, root) {
			return nil
		}
	}
	return fmt.Errorf("path %q is not below %s", mountPath, strings.Join(mountRoots, " or "))
}

// MountPathsOverlap reports whether two mount paths are the same or one is
// below the other.
func MountPathsOverlap(first, second string) bool {
	return first == second || strings.HasPrefix(second, first+"/") || strings.HasPrefix(first, second+"/")
}
