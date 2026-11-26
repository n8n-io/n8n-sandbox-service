// This file turns the runtime's terminal message into a task event, bounds
// the output manifest, and reads result.json from the task's output directory.

package codedaemon

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"syscall"
)

const inlineResultMax = 1 << 20 // 1 MiB; default and maximum inline result size.

var (
	errInvalidResult = errors.New("invalid result file")
	errOutputChanged = errors.New("output changed after publication")
	errOutputLimit   = errors.New("output limit exceeded")
	// errInvalidOutput rejects an output entry that is not a regular file or
	// a directory, such as a symlink, or that has an invalid path.
	errInvalidOutput = errors.New("invalid output file")
)

// resultEvent translates the worker's terminal message and validates successful outputs.
// The daemon does not look inside result.json. The runtime puts what the
// client needs there, such as the task's console output.
func (d *Daemon) resultEvent(spec taskSpec, root *os.Root, msg workerMessage) taskEvent {
	limit := spec.InlineResultMaxBytes
	if limit <= 0 || limit > inlineResultMax {
		limit = inlineResultMax
	}
	if msg.Type == messageTypeError {
		e := newErrorEvent(spec.TaskID, "user_error", "code execution failed")
		e.UserError = msg.Error
		// The error is the outcome, so a missing, large, or invalid
		// result.json only leaves the result out.
		if info, err := root.Lstat("result.json"); err == nil && info.Mode().IsRegular() && info.Size() <= limit {
			if result, err := readResult(root, outputEntry{Path: "result.json", Size: info.Size()}); err == nil {
				e.Result = result
			}
		}
		return e
	}
	outputs, err := collectOutputs(root, d.cfg.Limits)
	if err != nil {
		return newErrorEvent(spec.TaskID, "invalid", "invalid or oversized outputs")
	}
	result := json.RawMessage("null")
	for _, output := range outputs {
		if output.Path != "result.json" || output.Size > limit {
			continue
		}
		if result, err = readResult(root, output); err != nil {
			return newErrorEvent(spec.TaskID, "invalid", "invalid result file")
		}
	}
	return newDoneEvent(spec.TaskID, result, outputs)
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

// readResult reads a result.json that must still match its manifest entry and be valid JSON.
func readResult(root *os.Root, entry outputEntry) (json.RawMessage, error) {
	f, err := openOutput(root, entry)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, entry.Size+1))
	if err != nil || int64(len(data)) != entry.Size || !json.Valid(data) {
		return nil, errInvalidResult
	}
	return data, nil
}

// openOutput rechecks the file against the manifest without following a final
// symlink or blocking if the file was replaced by a FIFO.
func openOutput(root *os.Root, expected outputEntry) (*os.File, error) {
	f, err := root.OpenFile(expected.Path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Size {
		_ = f.Close()
		return nil, errOutputChanged
	}
	return f, nil
}
