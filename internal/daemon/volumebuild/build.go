package volumebuild

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// The builder's rootfs is read-only, so the image is mounted on a directory
// the sandbox image already has.
const mountPoint = "/mnt"

// ErrNotInit refuses build mode to anything but the init of a builder microVM.
var ErrNotInit = errors.New("volume build mode runs only as PID 1 and root")

// Sys is the system calls and tools a Builder uses; tests fake it.
type Sys interface {
	Pid() int
	Euid() int
	// Tar opens the first length bytes of device, or fails if it is shorter.
	Tar(device string, length int64) (io.ReadCloser, error)
	// Format makes device an empty ext4 file system with that many inodes.
	Format(device string, inodes int64) error
	// Mount mounts device at path, read-write.
	Mount(device, path string) error
	// UsedBytes is the space taken in the file system mounted at path.
	UsedBytes(path string) (int64, error)
	// Flush writes the file system at path to its device and reports any
	// write that failed.
	Flush(path string) error
	Unmount(path string) error
	// Check fails unless the file system on device is free of errors.
	Check(device string) error
	// WriteStatus writes record to the start of device, durably.
	WriteStatus(device string, record []byte) error
	// Reboot ends the microVM and returns only if that failed.
	Reboot() error
}

// Builder builds one volume image.
type Builder struct {
	sys        Sys
	mountPoint string
	tar        string
	output     string
	status     string
}

func New(sys Sys) *Builder {
	return &Builder{
		sys:        sys,
		mountPoint: mountPoint,
		tar:        devicePath(DriveTar),
		output:     devicePath(DriveOutput),
		status:     devicePath(DriveStatus),
	}
}

// Run is build mode. Unless the process is PID 1 and root, it returns
// ErrNotInit and has touched nothing. Otherwise it builds the image, records
// the outcome on the status drive whatever it is, and reboots; it then returns
// only if the reboot failed. paramsErr is ParamsFromEnv's error.
func (b *Builder) Run(params Params, paramsErr error) error {
	if b.sys.Pid() != 1 || b.sys.Euid() != 0 {
		return ErrNotInit
	}
	status := b.build(params, paramsErr)
	slog.Info("volume build finished", "result", status.Result, "reason", status.Reason,
		"files", status.Files, "bytes", status.Bytes, "used_bytes", status.UsedBytes,
		"format_ms", status.FormatMs, "extract_ms", status.ExtractMs, "sync_ms", status.SyncMs, "fsck_ms", status.FsckMs)

	record, err := status.encode()
	if err == nil {
		err = b.sys.WriteStatus(b.status, record)
	}
	if err != nil {
		slog.Error("write volume build status", "err", err)
	}
	if err := b.sys.Reboot(); err != nil {
		return fmt.Errorf("reboot after volume build: %w", err)
	}
	return nil
}

func (b *Builder) build(params Params, paramsErr error) (status Status) {
	status = Status{Version: statusVersion, Result: ResultFailed}
	// A panic would end PID 1, and the microVM, without a status record.
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("volume build panicked", "panic", recovered, "stack", string(debug.Stack()))
			status.Result, status.Reason = ResultFailed, ReasonInternal
		}
	}()
	fail := func(reason, step string, err error) Status {
		// The error may quote the tar; it goes to the console only.
		slog.Error("volume build failed", "step", step, "reason", reason, "err", err)
		status.Reason = reason
		return status
	}
	if paramsErr != nil {
		return fail(ReasonInternal, "parameters", paramsErr)
	}
	if err := params.Validate(); err != nil {
		return fail(ReasonInternal, "parameters", err)
	}

	drive, err := b.sys.Tar(b.tar, params.TarBytes)
	if err != nil {
		return fail(ReasonInternal, "open tar", err)
	}
	defer drive.Close()
	tar := &driveReader{drive: drive}

	start := time.Now()
	err = b.sys.Format(b.output, Inodes(params.Entries))
	status.FormatMs = time.Since(start).Milliseconds()
	if err != nil {
		return fail(ReasonFormat, "format", err)
	}

	start = time.Now()
	if err := b.sys.Mount(b.output, b.mountPoint); err != nil {
		return fail(ReasonMount, "mount", err)
	}
	stats, extractErr := b.extract(tar, params)
	status.ExtractMs = time.Since(start).Milliseconds()
	status.Files, status.Bytes = int64(stats.Files), stats.Bytes

	start = time.Now()
	var flushErr error
	if extractErr == nil {
		if status.UsedBytes, err = b.sys.UsedBytes(b.mountPoint); err != nil {
			slog.Warn("read space used by the volume", "err", err)
		}
		flushErr = b.sys.Flush(b.mountPoint)
	}
	// Always, so the file system check sees the drive closed.
	unmountErr := b.sys.Unmount(b.mountPoint)
	status.SyncMs = time.Since(start).Milliseconds()
	switch {
	case extractErr != nil:
		if unmountErr != nil {
			slog.Error("unmount after a failed unpack", "err", unmountErr)
		}
		if tar.err != nil {
			// The drive failed, not the tar on it.
			return fail(ReasonInternal, "read tar drive", tar.err)
		}
		return fail(extractReason(extractErr), "unpack", extractErr)
	case flushErr != nil:
		return fail(writeReason(flushErr), "flush", flushErr)
	case unmountErr != nil:
		return fail(ReasonMount, "unmount", unmountErr)
	}

	start = time.Now()
	err = b.sys.Check(b.output)
	status.FsckMs = time.Since(start).Milliseconds()
	if err != nil {
		return fail(ReasonFsck, "check", err)
	}

	status.Result = ResultOK
	return status
}

// driveReader remembers a failed read of the tar drive: the tar reader would
// report it as an invalid archive.
type driveReader struct {
	drive io.Reader
	err   error
}

func (d *driveReader) Read(buf []byte) (int, error) {
	read, err := d.drive.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		d.err = err
	}
	return read, err
}

// errOtherTar is a tar that unpacks but is not the one the parameters describe.
var errOtherTar = errors.New("tar does not match the build parameters")

func (b *Builder) extract(tar io.Reader, params Params) (safetar.Stats, error) {
	root, err := os.OpenRoot(b.mountPoint)
	if err != nil {
		return safetar.Stats{}, err
	}
	defer root.Close()

	stats, err := safetar.Extract(tar, root, volume.TarPolicy(int(params.Entries), params.ContentBytes, params.DecompressedBytes))
	if err != nil {
		return stats, err
	}
	// The caps only refuse more. Anything else than what the caller counted
	// is another tar too, and with no content a cap of 0 would not cap.
	if int64(stats.Entries) != params.Entries || stats.Bytes != params.ContentBytes || stats.DecompressedBytes != params.DecompressedBytes {
		return stats, fmt.Errorf("%w: %d entries, %d content bytes and %d decompressed bytes, want %d, %d and %d",
			errOtherTar, stats.Entries, stats.Bytes, stats.DecompressedBytes, params.Entries, params.ContentBytes, params.DecompressedBytes)
	}
	return stats, nil
}

func extractReason(err error) string {
	switch {
	case errors.Is(err, safetar.ErrTooLarge):
		return ReasonTooLarge
	case errors.Is(err, safetar.ErrInvalid), errors.Is(err, errOtherTar):
		return ReasonTar
	}
	var entryErr *safetar.EntryError
	if errors.As(err, &entryErr) {
		// Entries that do not fit together, such as a file below a file, or
		// names the file system does not take.
		for _, conflict := range []error{syscall.ENOTDIR, syscall.EISDIR, syscall.EEXIST, syscall.ENAMETOOLONG, syscall.EINVAL} {
			if errors.Is(err, conflict) {
				return ReasonTar
			}
		}
	}
	return writeReason(err)
}

func writeReason(err error) string {
	if errors.Is(err, syscall.ENOSPC) {
		return ReasonNoSpace
	}
	return ReasonInternal
}
