package volumecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// maxConcurrentUnpacks bounds the unpacks that run at once: each takes a CPU
// for gzip and writes to a disk all tenants on the runner share.
const maxConcurrentUnpacks = 2

// readDirBatch bounds the names settleTree reads from a directory at once.
const readDirBatch = 1024

// treeRoom is the most a tree unpacked within counts takes on disk, as the API
// counts it too.
func treeRoom(counts volume.TarCounts) room {
	treeBytes, treeInodes := counts.TreeRoom()
	return room{bytes: treeBytes, inodes: treeInodes}
}

// unpack unpacks the verified tar at tarPath into a tree at dest, within
// counts, and removes the tar whatever happens. It returns what the tree
// takes on disk. Waiting for a slot and the unpack itself run under the
// fetch's deadline, ctx.
func (c *Cache) unpack(ctx context.Context, key Key, counts volume.TarCounts, tarPath, dest string) (room, error) {
	defer func() { _ = os.Remove(tarPath) }()
	select {
	case c.unpackSlots <- struct{}{}:
	case <-ctx.Done():
		err := unpackFailure(ctx, ctx.Err())
		c.reportUnpack(ctx, key, room{}, 0, err)
		return room{}, err
	}
	defer func() { <-c.unpackSlots }()

	start := time.Now()
	tree, err := unpackTree(ctx, c.config.Dir, counts, tarPath, dest)
	if err != nil {
		err = unpackFailure(ctx, err)
	}
	c.reportUnpack(ctx, key, tree, time.Since(start), err)
	return tree, err
}

func (c *Cache) reportUnpack(ctx context.Context, key Key, tree room, dur time.Duration, err error) {
	if err != nil {
		result := unpackResultOf(err)
		c.config.Metrics.ObserveVolumeCacheUnpack(result)
		slog.WarnContext(ctx, "volume cache unpack failed",
			"scope", key.Scope, "sha256", key.SHA256, "result", result, "duration_ms", dur.Milliseconds(), "err", err)
		return
	}
	c.config.Metrics.ObserveVolumeCacheUnpack(metrics.VolumeUnpackSuccess)
	c.config.Metrics.ObserveVolumeCacheUnpackDuration(dur)
	slog.InfoContext(ctx, "volume cache unpack finished",
		"scope", key.Scope, "sha256", key.SHA256, "inodes", tree.inodes, "allocated_bytes", tree.bytes, "duration_ms", dur.Milliseconds())
}

// unpackTree unpacks the tar into a new directory in cacheDir and renames it to
// dest once it is complete. A failure leaves nothing behind; a crash leaves a
// dot file, which the startup wipe removes.
func unpackTree(ctx context.Context, cacheDir string, counts volume.TarCounts, tarPath, dest string) (room, error) {
	tarFile, err := os.Open(tarPath)
	if err != nil {
		return room{}, err
	}
	defer tarFile.Close()
	tmpDir, err := os.MkdirTemp(cacheDir, ".unpack-*")
	if err != nil {
		return room{}, err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	root, err := os.OpenRoot(tmpDir)
	if err != nil {
		return room{}, err
	}
	tree, err := extractAndSettle(ctx, root, counts, tarFile)
	if closeErr := root.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return room{}, err
	}
	if err := os.Rename(tmpDir, dest); err != nil {
		return room{}, err
	}
	renamed = true
	return tree, nil
}

func extractAndSettle(ctx context.Context, root *os.Root, counts volume.TarCounts, tar io.Reader) (room, error) {
	if _, err := safetar.Extract(contextReader{ctx: ctx, reader: tar}, root, counts.Policy()); err != nil {
		return room{}, err
	}
	return settleTree(ctx, root)
}

// settleTree gives every directory 0755 and every regular file 0644, or 0755
// if any execute bit is set, so a sandbox can read the tree and nothing more,
// and returns what the tree takes on disk. It works through roots, one per
// directory it descends into, so every lookup is a single name: it never
// follows a link, and its cost grows with the entries, not their depth. root
// is in the cache dir, which only the runner writes, and nothing is mounted
// from it yet.
func settleTree(ctx context.Context, root *os.Root) (room, error) {
	var tree room
	if _, err := settleEntry(root, ".", &tree); err != nil {
		return room{}, err
	}
	if err := settleDir(ctx, root, &tree); err != nil {
		return room{}, err
	}
	return tree, nil
}

// settleDir settles everything below dir, a root at one directory of the
// tree.
func settleDir(ctx context.Context, dir *os.Root, tree *room) error {
	subdirs, err := settleEntries(ctx, dir, tree)
	if err != nil {
		return err
	}
	for _, name := range subdirs {
		sub, err := dir.OpenRoot(name)
		if err != nil {
			return err
		}
		err = settleDir(ctx, sub, tree)
		if closeErr := sub.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// settleEntries settles the entries of dir, a batch of names at a time, and
// returns the names of its subdirectories.
func settleEntries(ctx context.Context, dir *os.Root, tree *room) ([]string, error) {
	file, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var subdirs []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := file.Readdirnames(readDirBatch)
		for _, name := range names {
			isDir, err := settleEntry(dir, name, tree)
			if err != nil {
				return nil, err
			}
			if isDir {
				subdirs = append(subdirs, name)
			}
		}
		if errors.Is(err, io.EOF) {
			return subdirs, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func settleEntry(root *os.Root, name string, tree *room) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return false, err
	}
	mode := info.Mode()
	var want fs.FileMode
	switch {
	case mode.IsDir():
		want = 0o755
	case mode.IsRegular() && mode&0o111 != 0:
		want = 0o755
	case mode.IsRegular():
		want = 0o644
	case mode.Type() != fs.ModeSymlink:
		return false, errors.New("the unpacked tree holds something other than directories, regular files and symbolic links")
	}
	if want != 0 && mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != want {
		if err := root.Chmod(name, want); err != nil {
			return false, err
		}
	}
	tree.bytes += allocatedBytes(info)
	tree.inodes++
	return mode.IsDir(), nil
}

// contextReader fails reads once ctx ends, which stops an unpack.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buf)
}

// unpackError is a failed unpack with its unpacks_total result. Its message
// names no path: names come from the tar, and the message can reach the
// client whose create failed. errors.Is finds is behind it: the safetar
// sentinel for a tar that breaks a rule, ErrCacheFull, or the context's error.
type unpackError struct {
	result string
	err    error
	is     error
}

func (e *unpackError) Error() string { return "unpack volume tar: " + e.err.Error() }

func (e *unpackError) Unwrap() error { return e.is }

func unpackResultOf(err error) string {
	var ue *unpackError
	if errors.As(err, &ue) {
		return ue.result
	}
	return metrics.VolumeUnpackIO
}

// unpackFailure classifies an unpack's error. A context error inside a tar
// error is a timeout, not a broken tar.
func unpackFailure(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return &unpackError{result: metrics.VolumeUnpackTimeout, err: errors.New("timed out"), is: ctx.Err()}
	case errors.Is(err, syscall.ENOSPC):
		return &unpackError{result: metrics.VolumeUnpackFull, err: fmt.Errorf("no space left on device: %w", ErrCacheFull), is: ErrCacheFull}
	case errors.Is(err, safetar.ErrTooLarge):
		return &unpackError{result: metrics.VolumeUnpackInvalid, err: withoutNames(err), is: safetar.ErrTooLarge}
	case errors.Is(err, safetar.ErrInvalid):
		return &unpackError{result: metrics.VolumeUnpackInvalid, err: withoutNames(err), is: safetar.ErrInvalid}
	default:
		return &unpackError{result: metrics.VolumeUnpackIO, err: withoutNames(err)}
	}
}

// withoutNames drops the entry name safetar adds, then any path.
func withoutNames(err error) error {
	var entryErr *safetar.EntryError
	if errors.As(err, &entryErr) {
		err = entryErr.Err
	}
	return withoutPath(err)
}
