// This file reserves task IDs and stages their input files for the shared runtime.

package codedaemon

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
)

var errDuplicateTask = errors.New("task ID already used")

// reserveTask rejects task IDs that are already reserved or have been dispatched.
func (d *Daemon) reserveTask(id string) error {
	d.tasksMu.Lock()
	defer d.tasksMu.Unlock()
	if _, exists := d.dispatchedByTaskID[id]; exists {
		return errDuplicateTask
	}
	d.dispatchedByTaskID[id] = false
	return nil
}

// releaseTask lets an ID be retried only if dispatch was never attempted.
func (d *Daemon) releaseTask(id string) {
	d.tasksMu.Lock()
	defer d.tasksMu.Unlock()
	if !d.dispatchedByTaskID[id] {
		delete(d.dispatchedByTaskID, id)
	}
}

// markDispatched keeps the ID reserved even if sending the start message fails.
func (d *Daemon) markDispatched(id string) {
	d.tasksMu.Lock()
	defer d.tasksMu.Unlock()
	d.dispatchedByTaskID[id] = true
}

func validFilePath(path string) bool {
	return path != "." && fs.ValidPath(path) && !strings.ContainsAny(path, "\\\x00")
}

// stageInputs creates a private task directory and removes it if staging fails.
func (d *Daemon) stageInputs(mr *multipart.Reader) (string, error) {
	dir, err := os.MkdirTemp(d.cfg.WorkDir, "task-")
	if err != nil {
		return "", err
	}
	if err := writeInputs(dir, mr); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// writeInputs confines multipart uploads to the task's input directory and rejects duplicate paths.
func writeInputs(dir string, mr *multipart.Reader) error {
	in := filepath.Join(dir, "in")
	for _, path := range []string{in, filepath.Join(dir, "out", "bin")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(in)
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Part.FileName strips directory components; the protocol preserves them.
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		rel := params["filename"]
		if err != nil || part.FormName() != "input" || !validFilePath(rel) {
			return errors.New("invalid input part")
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
			return err
		}
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, part)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
}
