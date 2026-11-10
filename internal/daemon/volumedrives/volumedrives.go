// Package volumedrives mounts the read-only volume drives the Firecracker
// runtime attaches to a sandbox. The guest sees drive N as /dev/vd followed by
// the letter b+N.
package volumedrives

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

const (
	// MaxRequestBytes bounds a mount request body. It fits volume.MaxDrives
	// volumes with the longest paths, even with every path byte JSON-escaped.
	MaxRequestBytes = 32 << 10

	sectorBytes = 512

	// The guest picks up a patched drive size asynchronously.
	sizeTimeout  = 2 * time.Second
	pollInterval = 10 * time.Millisecond
)

// Why MountAll failed, as Reason reports it.
const (
	ReasonDrive      = "drive"
	ReasonMountPoint = "mount_point"
	ReasonMount      = "mount"
)

// Volume is one drive and where to mount it.
type Volume struct {
	Drive   int    `json:"drive"`
	Sectors uint64 `json:"sectors"`
	Path    string `json:"path"`
}

func devicePath(drive int) string {
	return "/dev/vd" + string(rune('b'+drive))
}

// ParseRequest decodes and validates a mount request: {"volumes":[...]}.
func ParseRequest(body []byte) ([]Volume, error) {
	var req struct {
		Volumes []Volume `json:"volumes"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("request body must contain a single JSON value")
	}
	if err := validate(req.Volumes); err != nil {
		return nil, err
	}
	return req.Volumes, nil
}

func validate(volumes []Volume) error {
	if len(volumes) > volume.MaxDrives {
		return fmt.Errorf("at most %d volumes, got %d", volume.MaxDrives, len(volumes))
	}
	var seen [volume.MaxDrives]bool
	for index, vol := range volumes {
		if vol.Drive < 0 || vol.Drive >= volume.MaxDrives {
			return fmt.Errorf("volume %d: drive must be 0 to %d, got %d", index, volume.MaxDrives-1, vol.Drive)
		}
		if seen[vol.Drive] {
			return fmt.Errorf("volume %d: drive %d is listed twice", index, vol.Drive)
		}
		seen[vol.Drive] = true
		if vol.Sectors == 0 || vol.Sectors > math.MaxUint64/sectorBytes {
			return fmt.Errorf("volume %d: sectors out of range: %d", index, vol.Sectors)
		}
		if err := volume.ValidateMountPath(vol.Path); err != nil {
			return fmt.Errorf("volume %d: %w", index, err)
		}
		for _, prev := range volumes[:index] {
			if volume.MountPathsOverlap(prev.Path, vol.Path) {
				return fmt.Errorf("volume %d: path %q overlaps %q", index, vol.Path, prev.Path)
			}
		}
	}
	return nil
}

// Sys is the system calls a Mounter makes; tests fake it.
type Sys interface {
	DeviceSize(device string) (uint64, error)
	FlushBuffers(device string) error
	// MakeMountPoint creates path and missing parents, refusing a symlink, or a
	// directory another user owns or can write, on the path.
	MakeMountPoint(path string) error
	Mount(device, path string) error
	Unmount(path string) error
}

// Mounter mounts volume drives.
type Mounter struct {
	sys          Sys
	sizeTimeout  time.Duration
	pollInterval time.Duration
}

func NewMounter(sys Sys) *Mounter {
	return &Mounter{sys: sys, sizeTimeout: sizeTimeout, pollInterval: pollInterval}
}

// MountAll mounts all volumes or, after a failure, none.
func (m *Mounter) MountAll(volumes []Volume) error {
	mounted := make([]string, 0, len(volumes))
	for _, v := range volumes {
		if err := m.mountOne(v); err != nil {
			errs := []error{fmt.Errorf("volume drive %d at %s: %w", v.Drive, v.Path, err)}
			for i := len(mounted) - 1; i >= 0; i-- {
				if uerr := m.sys.Unmount(mounted[i]); uerr != nil {
					errs = append(errs, fmt.Errorf("roll back %s: %w", mounted[i], uerr))
				}
			}
			return errors.Join(errs...)
		}
		mounted = append(mounted, v.Path)
	}
	return nil
}

func (m *Mounter) mountOne(v Volume) error {
	device := devicePath(v.Drive)
	if err := m.waitForSize(device, v.Sectors*sectorBytes); err != nil {
		return &mountError{ReasonDrive, err}
	}
	if err := m.sys.FlushBuffers(device); err != nil {
		return &mountError{ReasonDrive, fmt.Errorf("flush buffers of %s: %w", device, err)}
	}
	if err := m.sys.MakeMountPoint(v.Path); err != nil {
		return &mountError{ReasonMountPoint, fmt.Errorf("create mount point: %w", err)}
	}
	if err := m.sys.Mount(device, v.Path); err != nil {
		return &mountError{ReasonMount, fmt.Errorf("mount %s: %w", device, err)}
	}
	return nil
}

// waitForSize waits for the size the runner patched in. Without it, some
// kernels skip end-of-device checks and a mount can appear to work.
func (m *Mounter) waitForSize(device string, want uint64) error {
	deadline := time.Now().Add(m.sizeTimeout)
	for {
		got, err := m.sys.DeviceSize(device)
		if err != nil {
			return fmt.Errorf("read size of %s: %w", device, err)
		}
		if got == want {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s has %d bytes after %s, want %d", device, got, m.sizeTimeout, want)
		}
		time.Sleep(m.pollInterval)
	}
}

type mountError struct {
	reason string
	err    error
}

func (e *mountError) Error() string { return e.err.Error() }
func (e *mountError) Unwrap() error { return e.err }

// Reason returns why a MountAll error failed, ReasonDrive, ReasonMountPoint or
// ReasonMount, or "" for another error.
func Reason(err error) string {
	var failed *mountError
	if errors.As(err, &failed) {
		return failed.reason
	}
	return ""
}

// makeMountPoint creates path and its missing parents below base, mode 0755.
// It refuses a path not clean or not below base, and an existing entry below
// base that is not a directory owned by owner and writable by no one else: no
// other user can then have put a symlink on the path, such as one left before a
// cold boot.
func makeMountPoint(base, path string, owner uint32) error {
	if path != filepath.Clean(path) {
		return fmt.Errorf("%s is not a clean path", path)
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s is not below %s", path, base)
	}
	current := base
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(current, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := checkDirectoryOnPath(current, info, owner); err != nil {
			return err
		}
	}
	return nil
}

func checkDirectoryOnPath(dir string, info fs.FileInfo, owner uint32) error {
	if !info.IsDir() {
		return fmt.Errorf("%s is a symlink or not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no owner to check", dir)
	}
	if stat.Uid != owner {
		return fmt.Errorf("%s is not owned by uid %d", dir, owner)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", dir)
	}
	return nil
}
