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
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	// MaxRequestBytes bounds a mount request body.
	MaxRequestBytes = 4 << 10

	maxVolumes   = 8
	maxPathBytes = 255
	sectorBytes  = 512

	// The guest picks up a patched drive size asynchronously.
	sizeTimeout  = 2 * time.Second
	pollInterval = 10 * time.Millisecond
)

var mountRoots = []string{"/mnt/", "/home/user/"}

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
	if len(volumes) > maxVolumes {
		return fmt.Errorf("at most %d volumes, got %d", maxVolumes, len(volumes))
	}
	var seen [maxVolumes]bool
	for i, v := range volumes {
		if v.Drive < 0 || v.Drive >= maxVolumes {
			return fmt.Errorf("volume %d: drive must be 0 to %d, got %d", i, maxVolumes-1, v.Drive)
		}
		if seen[v.Drive] {
			return fmt.Errorf("volume %d: drive %d is listed twice", i, v.Drive)
		}
		seen[v.Drive] = true
		if v.Sectors == 0 || v.Sectors > math.MaxUint64/sectorBytes {
			return fmt.Errorf("volume %d: sectors out of range: %d", i, v.Sectors)
		}
		if err := validatePath(v.Path); err != nil {
			return fmt.Errorf("volume %d: %w", i, err)
		}
		for _, prev := range volumes[:i] {
			if pathsOverlap(prev.Path, v.Path) {
				return fmt.Errorf("volume %d: path %q overlaps %q", i, v.Path, prev.Path)
			}
		}
	}
	return nil
}

func validatePath(p string) error {
	if len(p) > maxPathBytes {
		return fmt.Errorf("path is longer than %d bytes", maxPathBytes)
	}
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("path %q is not absolute and clean", p)
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("path %q contains a control character", p)
	}
	for _, root := range mountRoots {
		if strings.HasPrefix(p, root) {
			return nil
		}
	}
	return fmt.Errorf("path %q is not below %s", p, strings.Join(mountRoots, " or "))
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// Sys is the system calls a Mounter makes; tests fake it.
type Sys interface {
	DeviceSize(device string) (uint64, error)
	FlushBuffers(device string) error
	// MakeMountPoint creates path and missing parents, refusing a symlink on it.
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
		return &mountError{"drive", err}
	}
	if err := m.sys.FlushBuffers(device); err != nil {
		return &mountError{"drive", fmt.Errorf("flush buffers of %s: %w", device, err)}
	}
	if err := m.sys.MakeMountPoint(v.Path); err != nil {
		return &mountError{"mount_point", fmt.Errorf("create mount point: %w", err)}
	}
	if err := m.sys.Mount(device, v.Path); err != nil {
		return &mountError{"mount", fmt.Errorf("mount %s: %w", device, err)}
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

// Reason returns why a MountAll error failed, "drive", "mount_point" or
// "mount", or "" for another error.
func Reason(err error) string {
	var failed *mountError
	if errors.As(err, &failed) {
		return failed.reason
	}
	return ""
}

// mkdirNoFollow creates dir and missing parents, mode 0755 before the umask,
// and fails on a symlink anywhere on it, such as one a tenant left in
// /home/user before a cold boot.
func mkdirNoFollow(dir string) error {
	cur := "/"
	for _, name := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		cur = filepath.Join(cur, name)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is a symlink or not a directory", cur)
		}
	}
	return nil
}
