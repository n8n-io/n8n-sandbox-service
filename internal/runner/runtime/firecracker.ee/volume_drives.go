package firecracker

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

const volumeDrivePlaceholderName = "volume-drive-placeholder"

// volumeDrivePlaceholders hard-links each volume drive's jail path to one empty,
// root-owned, read-only file: no mounts, and one inode for the drive serial.
// Links are renamed in from the root-owned jail directory, because Firecracker's
// uid can write to the jail root and a rename replaces a planted symlink.
func volumeDrivePlaceholders(jailerBaseDir, jailRoot string, drives []volumeDrive) string {
	if len(drives) == 0 {
		return ""
	}
	placeholder := shellquote.Quote(filepath.Join(jailerBaseDir, volumeDrivePlaceholderName))
	var script strings.Builder
	fmt.Fprintf(&script, "if [ ! -f %[1]s ]; then (umask 0333 && : > %[1]s.$$ && mv -fT %[1]s.$$ %[1]s); fi\n", placeholder)
	for _, drive := range drives {
		name := strings.TrimPrefix(drive.Path, "/")
		staged := shellquote.Quote(filepath.Join(filepath.Dir(jailRoot), "."+name))
		fmt.Fprintf(&script, "ln -f %s %s\nmv -fT %s %s\n", placeholder, staged, staged, shellquote.Quote(filepath.Join(jailRoot, name)))
	}
	return script.String()
}
