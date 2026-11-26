// This file stages a task's input files in a private task directory.

package codedaemon

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	errInputLimit       = errors.New("input limit exceeded")
	errInvalidInputPart = errors.New("invalid input part")
)

// maxDirs bounds the directories that a task's inputs create and that are
// walked to list its outputs, separately from the file limit so nested paths
// don't use it up.
const maxDirs = 4096

func validFilePath(path string) bool {
	return len(path) <= 1024 && path != "." && fs.ValidPath(path) && !strings.ContainsAny(path, "\\\x00")
}

// stageInputs creates a private task directory and removes it if staging fails.
func (d *Daemon) stageInputs(mr *multipart.Reader) (string, error) {
	dir, err := os.MkdirTemp(d.cfg.WorkDir, "task-")
	if err != nil {
		return "", err
	}
	if err := writeInputs(dir, mr, d.cfg.Limits); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// writeInputs stages files within the task's byte, file and directory limits, without overwriting files.
func writeInputs(dir string, mr *multipart.Reader, limits Limits) error {
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
	remaining := int64(limits.InputBytes)
	files := 0
	dirs := make(map[string]struct{})
	for {
		part, err := mr.NextPart()
		// NextPart returns a bare io.EOF only after the closing boundary. A
		// wrapped io.EOF means the upload was cut, which must fail.
		if err == io.EOF { //nolint:errorlint // See above.
			return nil
		}
		if err != nil {
			return err
		}
		// Part.FileName strips directory components; the protocol preserves them.
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		rel := params["filename"]
		if err != nil || part.FormName() != "input" || !validFilePath(rel) {
			return errInvalidInputPart
		}
		files++
		if files > limits.Files || !addParentDirs(dirs, rel) {
			return errInputLimit
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
			return err
		}
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(part, remaining))
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
		remaining -= n
		if remaining == 0 {
			// A limited copy cannot tell whether the part ends exactly at the limit.
			var probe [1]byte
			if _, err := io.ReadFull(part, probe[:]); err != io.EOF {
				if err != nil {
					return err
				}
				return errInputLimit
			}
		}
	}
}

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
