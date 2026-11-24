// This file defines task limits and bounds output manifests.
package codedaemon

import (
	"errors"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

var (
	errInputLimit  = errors.New("input limit exceeded")
	errOutputLimit = errors.New("output limit exceeded")
	// errInvalidOutput rejects an output entry that is not a regular file or
	// a directory, such as a symlink, or that has an invalid path.
	errInvalidOutput = errors.New("invalid output file")
)

// Limits caps aggregate input and output bytes and file counts in each direction.
type Limits struct {
	InputBytes, OutputBytes, Files int
}

// withDefaults fills in limits that were not set to a positive value.
func (l Limits) withDefaults() Limits {
	if l.InputBytes <= 0 {
		l.InputBytes = 64 << 20 // 64 MiB
	}
	if l.OutputBytes <= 0 {
		l.OutputBytes = 64 << 20 // 64 MiB
	}
	if l.Files <= 0 {
		l.Files = 256
	}
	return l
}

// loadLimits validates environment overrides and uses defaults for unset limits.
func loadLimits() (Limits, error) {
	l := (Limits{}).withDefaults()
	for _, item := range []struct {
		key   string
		value *int
		max   int
	}{
		{"CODE_DAEMON_MAX_INPUT_BYTES", &l.InputBytes, 1 << 30},   // 1 GiB
		{"CODE_DAEMON_MAX_OUTPUT_BYTES", &l.OutputBytes, 1 << 30}, // 1 GiB
		{"CODE_DAEMON_MAX_FILES", &l.Files, 4096},
	} {
		if err := envvar.IntBetween(item.key, item.value, 1, item.max); err != nil {
			return Limits{}, err
		}
	}
	return l, nil
}

// maxDirs bounds the directories that a task's inputs create and that are
// walked to list its outputs, separately from the file limit so nested paths
// don't use it up.
const maxDirs = 4096

// addParentDirs records the parent directories of rel in dirs and reports
// whether dirs is still within maxDirs.
func addParentDirs(dirs map[string]struct{}, rel string) bool {
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		if _, ok := dirs[dir]; ok {
			break // its parents are already recorded
		}
		dirs[dir] = struct{}{}
	}
	return len(dirs) <= maxDirs
}

type outputEntry struct {
	Path string `json:"path"` // relative to the task's output directory
	Size int64  `json:"size"`
}

// collectOutputs bounds the manifest for task output publication; it does not cap disk writes.
func collectOutputs(root *os.Root, limits Limits) ([]outputEntry, error) {
	entries := make([]outputEntry, 0)
	var bytes int64
	seenDirs := 0
	dirs := []string{"."}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		// Every entry uses up a file or a directory, so reading one more than
		// both remaining budgets is enough to detect overflow.
		children, err := f.ReadDir(limits.Files - len(entries) + maxDirs - seenDirs + 1)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, entry := range children {
			rel := path.Join(dir, entry.Name())
			if !validFilePath(rel) {
				return nil, errInvalidOutput
			}
			if entry.IsDir() {
				if seenDirs++; seenDirs > maxDirs {
					return nil, errOutputLimit
				}
				dirs = append(dirs, rel)
				continue
			}
			if !entry.Type().IsRegular() {
				return nil, errInvalidOutput
			}
			if len(entries) == limits.Files {
				return nil, errOutputLimit
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, errInvalidOutput
			}
			if info.Size() > int64(limits.OutputBytes)-bytes {
				return nil, errOutputLimit
			}
			bytes += info.Size()
			entries = append(entries, outputEntry{Path: rel, Size: info.Size()})
		}
	}
	return entries, nil
}
