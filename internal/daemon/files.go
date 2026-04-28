package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// HandleFileList returns directory entries for the directory at path inside base.
// If recursive is true, it walks the tree. If extension is non-empty, only files
// matching that extension are returned.
func HandleFileList(base, path string, recursive bool, extension string) ([]FileInfo, error) {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		return nil, fmt.Errorf("file_list resolve: %w", err)
	}

	if !recursive {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return nil, fmt.Errorf("file_list readdir %s: %w", resolved, err)
		}

		infos := make([]FileInfo, 0, len(entries))
		for _, entry := range entries {
			if extension != "" && !entry.IsDir() && !strings.HasSuffix(entry.Name(), extension) {
				continue
			}
			fi, err := entry.Info()
			if err != nil {
				continue
			}
			infos = append(infos, fileInfoFrom(entry.Name(), fi))
		}
		return infos, nil
	}

	// Recursive walk
	var infos []FileInfo
	err = filepath.WalkDir(resolved, func(walkPath string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip entries we cannot access
		}
		// Skip the root directory itself
		if walkPath == resolved {
			return nil
		}
		if extension != "" && !d.IsDir() && !strings.HasSuffix(d.Name(), extension) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		// Use path relative to the resolved directory
		relPath, _ := filepath.Rel(resolved, walkPath)
		infos = append(infos, fileInfoFrom(relPath, fi))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("file_list walk %s: %w", resolved, err)
	}
	if infos == nil {
		infos = []FileInfo{}
	}
	return infos, nil
}

// HandleFileRead reads up to maxBytes from the file at path inside base.
// If maxBytes is <= 0, no size limit is enforced.
func HandleFileRead(base, path string, maxBytes int64) ([]byte, error) {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		return nil, fmt.Errorf("file_read resolve: %w", err)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("file_read open %s: %w", resolved, err)
	}
	defer f.Close()

	if maxBytes > 0 {
		// Check size before reading.
		fi, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("file_read stat %s: %w", resolved, err)
		}
		if fi.Size() > maxBytes {
			return nil, fmt.Errorf("file_read: file size %d exceeds limit %d", fi.Size(), maxBytes)
		}
		data := make([]byte, fi.Size())
		_, err = io.ReadFull(f, data)
		if err != nil {
			return nil, fmt.Errorf("file_read read %s: %w", resolved, err)
		}
		return data, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("file_read read %s: %w", resolved, err)
	}
	return data, nil
}

// HandleFileWrite writes data to path inside base, creating intermediate
// directories as needed. If maxBytes is > 0, the write is rejected when
// len(data) exceeds maxBytes. If overwrite is false and the file already exists,
// an error is returned.
func HandleFileWrite(base, path string, data []byte, maxBytes int64, overwrite bool) error {
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return fmt.Errorf("file_write: data size %d exceeds limit %d", len(data), maxBytes)
	}

	resolved, err := SafeResolve(base, path)
	if err != nil {
		return fmt.Errorf("file_write resolve: %w", err)
	}

	// Ensure parent directory exists.
	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("file_write mkdir %s: %w", dir, err)
	}

	if overwrite {
		if err := os.WriteFile(resolved, data, 0o644); err != nil {
			return fmt.Errorf("file_write write %s: %w", resolved, err)
		}
		return nil
	}

	// Atomic create-only write when overwrite is disabled.
	f, err := os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("file_write: file already exists: %s", path)
		}
		return fmt.Errorf("file_write open %s: %w", resolved, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("file_write write %s: %w", resolved, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("file_write close %s: %w", resolved, err)
	}
	return nil
}

// HandleFileAppend appends data to the file at path inside base, creating the
// file and intermediate directories if they don't exist.
func HandleFileAppend(base, path string, data []byte) error {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		return fmt.Errorf("file_append resolve: %w", err)
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("file_append mkdir %s: %w", dir, err)
	}

	f, err := os.OpenFile(resolved, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("file_append open %s: %w", resolved, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("file_append write %s: %w", resolved, err)
	}
	return nil
}

// HandleFileDelete removes the file or directory at path inside base.
// If recursive is true, it removes non-empty directories. If force is true,
// "not found" errors are ignored.
func HandleFileDelete(base, path string, recursive, force bool) error {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		if force && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("file_delete resolve: %w", err)
	}

	var removeErr error
	if recursive {
		removeErr = os.RemoveAll(resolved)
	} else {
		removeErr = os.Remove(resolved)
	}

	if removeErr != nil {
		if force && os.IsNotExist(removeErr) {
			return nil
		}
		return fmt.Errorf("file_delete remove %s: %w", resolved, removeErr)
	}
	return nil
}

// HandleFileCopy copies a file or directory from srcPath to destPath inside base.
// If recursive is true, directories are copied recursively. If overwrite is false
// and the destination exists, an error is returned.
func HandleFileCopy(base, srcPath, destPath string, recursive, overwrite bool) error {
	resolvedSrc, err := SafeResolve(base, srcPath)
	if err != nil {
		return fmt.Errorf("file_copy resolve src: %w", err)
	}
	resolvedDest, err := SafeResolve(base, destPath)
	if err != nil {
		return fmt.Errorf("file_copy resolve dest: %w", err)
	}

	srcInfo, err := os.Stat(resolvedSrc)
	if err != nil {
		return fmt.Errorf("file_copy stat src %s: %w", resolvedSrc, err)
	}

	if !overwrite {
		if _, err := os.Stat(resolvedDest); err == nil {
			return fmt.Errorf("file_copy: destination already exists: %s", destPath)
		}
	}

	if srcInfo.IsDir() {
		if !recursive {
			return fmt.Errorf("file_copy: source is a directory, use recursive option")
		}
		if isSameOrSubpath(resolvedSrc, resolvedDest) {
			return fmt.Errorf("file_copy: destination must not be inside source directory")
		}
		return copyDir(resolvedSrc, resolvedDest)
	}

	return copyFile(resolvedSrc, resolvedDest)
}

// HandleFileMove moves a file or directory from srcPath to destPath inside base.
// If overwrite is false and the destination exists, an error is returned.
func HandleFileMove(base, srcPath, destPath string, overwrite bool) error {
	resolvedSrc, err := SafeResolve(base, srcPath)
	if err != nil {
		return fmt.Errorf("file_move resolve src: %w", err)
	}
	resolvedDest, err := SafeResolve(base, destPath)
	if err != nil {
		return fmt.Errorf("file_move resolve dest: %w", err)
	}

	if !overwrite {
		if _, err := os.Stat(resolvedDest); err == nil {
			return fmt.Errorf("file_move: destination already exists: %s", destPath)
		}
	}

	srcInfo, err := os.Stat(resolvedSrc)
	if err != nil {
		return fmt.Errorf("file_move stat src %s: %w", resolvedSrc, err)
	}
	if srcInfo.IsDir() && isSameOrSubpath(resolvedSrc, resolvedDest) {
		return fmt.Errorf("file_move: destination must not be inside source directory")
	}

	// Ensure parent directory of destination exists.
	if err := os.MkdirAll(filepath.Dir(resolvedDest), 0o755); err != nil {
		return fmt.Errorf("file_move mkdir dest: %w", err)
	}

	if err := os.Rename(resolvedSrc, resolvedDest); err != nil {
		var linkErr *os.LinkError
		if !errors.As(err, &linkErr) || !errors.Is(linkErr.Err, syscall.EXDEV) {
			return fmt.Errorf("file_move rename %s -> %s: %w", resolvedSrc, resolvedDest, err)
		}

		// Cross-device move: fall back to copy + delete.
		if srcInfo.IsDir() {
			if cpErr := copyDir(resolvedSrc, resolvedDest); cpErr != nil {
				return fmt.Errorf("file_move copy fallback: %w", cpErr)
			}
		} else {
			if cpErr := copyFile(resolvedSrc, resolvedDest); cpErr != nil {
				return fmt.Errorf("file_move copy fallback: %w", cpErr)
			}
		}
		if rmErr := os.RemoveAll(resolvedSrc); rmErr != nil {
			return fmt.Errorf("file_move remove src after copy: %w", rmErr)
		}
	}
	return nil
}

// HandleFileMkdir creates a directory at path inside base. If recursive is true,
// intermediate directories are created as needed.
func HandleFileMkdir(base, path string, recursive bool) error {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		return fmt.Errorf("file_mkdir resolve: %w", err)
	}

	if recursive {
		if err := os.MkdirAll(resolved, 0o755); err != nil {
			return fmt.Errorf("file_mkdir mkdirall %s: %w", resolved, err)
		}
	} else {
		if err := os.Mkdir(resolved, 0o755); err != nil {
			return fmt.Errorf("file_mkdir mkdir %s: %w", resolved, err)
		}
	}
	return nil
}

// HandleFileStat returns metadata about the file or directory at path inside base.
func HandleFileStat(base, path string) (*FileStatInfo, error) {
	resolved, err := SafeResolve(base, path)
	if err != nil {
		return nil, fmt.Errorf("file_stat resolve: %w", err)
	}

	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("file_stat stat %s: %w", resolved, err)
	}

	fileType := "file"
	if fi.IsDir() {
		fileType = "directory"
	}

	return &FileStatInfo{
		Name:       fi.Name(),
		Path:       path,
		Type:       fileType,
		Size:       fi.Size(),
		CreatedAt:  fi.ModTime(), // birth time not reliably available on Linux; use mod time
		ModifiedAt: fi.ModTime(),
	}, nil
}

// fileInfoFrom constructs a FileInfo from a name and os.FileInfo.
func fileInfoFrom(name string, fi os.FileInfo) FileInfo {
	fileType := "file"
	if fi.IsDir() {
		fileType = "directory"
	}
	return FileInfo{
		Name:    name,
		Size:    fi.Size(),
		IsDir:   fi.IsDir(),
		Type:    fileType,
		ModTime: fi.ModTime(),
	}
}

// copyFile copies a single file from src to dest, creating parent directories as needed.
func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	destFile, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}

	if _, err := io.Copy(destFile, srcFile); err != nil {
		_ = destFile.Close()
		return err
	}
	if err := destFile.Close(); err != nil {
		return err
	}
	return nil
}

// copyDir recursively copies a directory tree from src to dest.
func copyDir(src, dest string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dest, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		destPath := filepath.Join(dest, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, destPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, destPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// isSameOrSubpath reports whether candidate is the same path as root or is nested under it.
func isSameOrSubpath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
