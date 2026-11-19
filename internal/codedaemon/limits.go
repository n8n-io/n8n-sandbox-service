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
)

// Limits caps aggregate input and output bytes, file counts in each direction,
// and the console entries and bytes retained per task.
type Limits struct {
	InputBytes, OutputBytes, Files, ConsoleEntries, ConsoleBytes int
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
	if l.ConsoleEntries <= 0 {
		l.ConsoleEntries = 1000
	}
	if l.ConsoleBytes <= 0 {
		l.ConsoleBytes = 16 << 10 // 16 KiB
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
		{"CODE_DAEMON_MAX_CONSOLE_ENTRIES", &l.ConsoleEntries, 100_000},
		{"CODE_DAEMON_MAX_CONSOLE_BYTES", &l.ConsoleBytes, 16 << 20}, // 16 MiB
	} {
		if err := envvar.IntBetween(item.key, item.value, 1, item.max); err != nil {
			return Limits{}, err
		}
	}
	return l, nil
}

// maxOutputDirs bounds the directories walked to list a task's outputs,
// separately from the file limit so nested output paths don't use it up.
const maxOutputDirs = 4096

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
		children, err := f.ReadDir(limits.Files - len(entries) + maxOutputDirs - seenDirs + 1)
		_ = f.Close()
		if err != nil && err != io.EOF {
			return nil, err
		}
		for _, entry := range children {
			rel := path.Join(dir, entry.Name())
			if !validFilePath(rel) {
				return nil, errOutputLimit
			}
			if entry.IsDir() {
				if seenDirs++; seenDirs > maxOutputDirs {
					return nil, errOutputLimit
				}
				dirs = append(dirs, rel)
				continue
			}
			if !entry.Type().IsRegular() || len(entries) == limits.Files {
				return nil, errOutputLimit
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() || info.Size() > int64(limits.OutputBytes)-bytes {
				return nil, errOutputLimit
			}
			bytes += info.Size()
			entries = append(entries, outputEntry{Path: rel, Size: info.Size()})
		}
	}
	return entries, nil
}
