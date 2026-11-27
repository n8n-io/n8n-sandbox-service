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

// Why a volume did not mount, as an Outcome reports it.
const (
	ReasonDrive      = "drive"
	ReasonMountPoint = "mount_point"
	ReasonMount      = "mount"
)

// KnownReason reports whether reason is one of the Reason values.
func KnownReason(reason string) bool {
	return reason == ReasonDrive || reason == ReasonMountPoint || reason == ReasonMount
}

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

// Outcome is how one volume of a mount request went: mounted, or why not.
type Outcome struct {
	Drive   int    `json:"drive"`
	Mounted bool   `json:"mounted"`
	Reason  string `json:"reason,omitempty"`
}

// Answer is the answer to a mount request with volumes.
type Answer struct {
	Volumes []Outcome `json:"volumes"`
}

// MaxAnswerBytes bounds an Answer.
const MaxAnswerBytes = 4 << 10

// ParseAnswer decodes the answer to a mount request with volumes,
// {"volumes":[...]}, and checks it against the request: one outcome per
// requested volume, in request order, with a known reason for each that did
// not mount and none for each that did.
func ParseAnswer(body []byte, requested []Volume) ([]Outcome, error) {
	if len(body) > MaxAnswerBytes {
		return nil, fmt.Errorf("answer is longer than %d bytes", MaxAnswerBytes)
	}
	var answer Answer
	// Decode errors are left out: they can quote the answer.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&answer) != nil {
		return nil, errors.New("answer is not a JSON object of volume outcomes")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("answer must contain a single JSON value")
	}
	if len(answer.Volumes) != len(requested) {
		return nil, fmt.Errorf("answer has %d volumes, the request %d", len(answer.Volumes), len(requested))
	}
	for index, outcome := range answer.Volumes {
		if outcome.Drive != requested[index].Drive {
			return nil, fmt.Errorf("answer volume %d is drive %d, the request's is drive %d", index, outcome.Drive, requested[index].Drive)
		}
		consistent := outcome.Mounted && outcome.Reason == "" || !outcome.Mounted && KnownReason(outcome.Reason)
		if !consistent {
			return nil, fmt.Errorf("answer volume %d has an unknown reason or one that contradicts mounted", index)
		}
	}
	return answer.Volumes, nil
}

// Sys is the system calls a Mounter makes; tests fake it.
type Sys interface {
	DeviceSize(device string) (uint64, error)
	FlushBuffers(device string) error
	// MakeMountPoint creates path and missing parents, refusing a symlink, or a
	// directory another user owns or can write, on the path.
	MakeMountPoint(path string) error
	Mount(device, path string) error
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

// MountEach mounts each volume on its own, in order: one that fails leaves the
// others as they are. It returns an outcome per volume, and the failures, with
// details from the guest, joined.
func (m *Mounter) MountEach(volumes []Volume) ([]Outcome, error) {
	outcomes := make([]Outcome, 0, len(volumes))
	var errs []error
	for _, vol := range volumes {
		if reason, err := m.mountOne(vol); err != nil {
			errs = append(errs, fmt.Errorf("volume drive %d at %s: %w", vol.Drive, vol.Path, err))
			outcomes = append(outcomes, Outcome{Drive: vol.Drive, Reason: reason})
			continue
		}
		outcomes = append(outcomes, Outcome{Drive: vol.Drive, Mounted: true})
	}
	return outcomes, errors.Join(errs...)
}

// mountOne mounts one volume, or returns why not and the error.
func (m *Mounter) mountOne(vol Volume) (string, error) {
	device := devicePath(vol.Drive)
	if err := m.waitForSize(device, vol.Sectors*sectorBytes); err != nil {
		return ReasonDrive, err
	}
	if err := m.sys.FlushBuffers(device); err != nil {
		return ReasonDrive, fmt.Errorf("flush buffers of %s: %w", device, err)
	}
	if err := m.sys.MakeMountPoint(vol.Path); err != nil {
		return ReasonMountPoint, fmt.Errorf("create mount point: %w", err)
	}
	if err := m.sys.Mount(device, vol.Path); err != nil {
		return ReasonMount, fmt.Errorf("mount %s: %w", device, err)
	}
	return "", nil
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
