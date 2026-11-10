// Package safetar unpacks untrusted tar archives, plain or gzip-compressed, or
// checks them under the same rules without unpacking.
//
// Entry paths must be relative and free of .. components. Hard links, devices,
// FIFOs and sparse files are always refused. Owners, groups and extended
// attributes in the archive are never applied. No entry may go through or
// replace a symbolic link that an earlier entry created; names are compared
// byte for byte, so this holds on case-sensitive file systems only.
package safetar

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalid marks an archive that cannot be read, or an entry the policy does
// not accept.
var ErrInvalid = errors.New("invalid archive")

// ErrTooLarge marks an archive with more entries or more file content than the
// policy allows.
var ErrTooLarge = errors.New("archive too large")

// EntryError is a failure at one entry of an archive.
type EntryError struct {
	Name string
	Err  error
}

func (e *EntryError) Error() string { return fmt.Sprintf("entry %q: %v", e.Name, e.Err) }

func (e *EntryError) Unwrap() error { return e.Err }

// Policy is what an archive may hold beyond directories and regular files.
// A cap of 0 means no cap; a negative cap refuses every archive.
type Policy struct {
	// AllowSymlinks accepts links to any non-empty target. It needs
	// MaxEntries, which bounds the memory spent tracking links.
	AllowSymlinks bool
	// LocalSymlinks refuses link targets that are absolute or contain ..,
	// so no link points outside a tree that held no links before. Without
	// it, access the unpacked tree only through an os.Root.
	LocalSymlinks bool
	// KeepExecBit gives a new file 0755 instead of 0644 if the archive marks
	// it executable.
	KeepExecBit bool
	// MaxEntries counts every entry, directories and pax global headers
	// included.
	MaxEntries int
	// MaxBytes counts regular-file content.
	MaxBytes int64
	// MaxTarBytes counts the whole tar stream after decompression: headers,
	// padding and anything after the end of the archive included. It does
	// not bound the compressed input.
	MaxTarBytes int64
}

// Stats counts regular files only.
type Stats struct {
	Files int
	Bytes int64
}

// Extract unpacks an archive into root, creating parent directories and
// overwriting files as needed. New directories get 0755 and new files 0644,
// less the umask; existing ones keep their mode. The first failure stops the
// unpack and leaves earlier entries in place.
//
// Errors reading the archive wrap ErrInvalid; file-system errors do not.
func Extract(r io.Reader, root *os.Root, p Policy) (Stats, error) {
	return unpack(r, p, extractor{root})
}

// Validate reads an archive to its end and refuses what Extract would refuse,
// except file-system conflicts, such as a file and a directory with the same
// name.
func Validate(r io.Reader, p Policy) (Stats, error) {
	return unpack(r, p, validator{})
}

type sink interface {
	mkdir(name string) error
	file(name string, perm os.FileMode, content io.Reader) (int64, error)
	symlink(target, name string) error
}

func unpack(r io.Reader, p Policy, s sink) (Stats, error) {
	var stats Stats
	if p.MaxEntries < 0 || p.MaxBytes < 0 || p.MaxTarBytes < 0 {
		return stats, fmt.Errorf("%w: negative cap", ErrTooLarge)
	}
	if p.AllowSymlinks && p.MaxEntries == 0 {
		return stats, errors.New("AllowSymlinks needs MaxEntries")
	}
	src, err := gunzipIfCompressed(r)
	if err != nil {
		return stats, invalid(err)
	}
	if p.MaxTarBytes > 0 {
		src = &cappedReader{r: src, max: p.MaxTarBytes}
	}
	tr := tar.NewReader(src)
	links := symlinks{}
	for entries := 1; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// The tar ends before the gzip stream does: reading on to the end is
			// what checks the gzip footer.
			if _, err := io.Copy(io.Discard, src); err != nil {
				return stats, invalid(err)
			}
			return stats, nil
		}
		if err != nil {
			return stats, invalid(err)
		}
		if p.MaxEntries > 0 && entries > p.MaxEntries {
			return stats, fmt.Errorf("%w: more than %d entries", ErrTooLarge, p.MaxEntries)
		}

		// A pax global header holds metadata for the whole archive, not a file.
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name := hdr.Name
		if err := check(hdr, p, links); err != nil {
			return stats, &EntryError{Name: name, Err: err}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := s.mkdir(name); err != nil {
				return stats, &EntryError{Name: name, Err: err}
			}
		case tar.TypeReg:
			// Subtracting keeps the sum from overflowing.
			if p.MaxBytes > 0 && hdr.Size > p.MaxBytes-stats.Bytes {
				return stats, &EntryError{Name: name, Err: fmt.Errorf("%w: files exceed %d bytes", ErrTooLarge, p.MaxBytes)}
			}
			n, err := s.file(name, filePerm(hdr, p), entryReader{tr})
			if err != nil {
				return stats, &EntryError{Name: name, Err: err}
			}
			stats.Files++
			stats.Bytes += n
		case tar.TypeSymlink:
			if err := s.symlink(hdr.Linkname, name); err != nil {
				return stats, &EntryError{Name: name, Err: err}
			}
			links.add(filepath.Clean(name))
		}
	}
}

func check(hdr *tar.Header, p Policy, links symlinks) error {
	name := hdr.Name
	if !filepath.IsLocal(name) || hasDotDot(name) {
		return fmt.Errorf("%w: path must be relative and must not contain ..", ErrInvalid)
	}
	// The reader hands pax sparse files over as regular ones and fills their
	// holes with zeros the archive does not carry. Refusing them keeps what is
	// written to what the archive holds.
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return fmt.Errorf("%w: sparse files are not allowed", ErrInvalid)
		}
	}

	switch {
	case hdr.Typeflag == tar.TypeDir, hdr.Typeflag == tar.TypeReg:
	case hdr.Typeflag == tar.TypeSymlink && p.AllowSymlinks:
		if hdr.Linkname == "" {
			return fmt.Errorf("%w: symbolic link target must not be empty", ErrInvalid)
		}
		if p.LocalSymlinks && (!filepath.IsLocal(hdr.Linkname) || hasDotDot(hdr.Linkname)) {
			return fmt.Errorf("%w: symbolic link target must be relative and must not contain ..", ErrInvalid)
		}
	case p.AllowSymlinks:
		return fmt.Errorf("%w: only regular files, directories and symbolic links are allowed", ErrInvalid)
	default:
		return fmt.Errorf("%w: only regular files and directories are allowed", ErrInvalid)
	}

	// Validate cannot tell where such a link points.
	if links.crosses(filepath.Clean(name)) {
		return fmt.Errorf("%w: path must not go through or replace a symbolic link from the archive", ErrInvalid)
	}
	return nil
}

func hasDotDot(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func filePerm(hdr *tar.Header, p Policy) os.FileMode {
	if p.KeepExecBit && hdr.Mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// symlinks holds FNV-1a hashes of the cleaned paths of the links an archive
// created, so memory does not grow with name length. A collision can only
// refuse an entry.
type symlinks map[uint64]struct{}

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

// crosses reports whether the cleaned path name is a link in s or lies under
// one.
func (s symlinks) crosses(name string) bool {
	if len(s) == 0 {
		return false
	}
	h := uint64(fnvOffset)
	for i := range len(name) {
		// h is the hash of name[:i].
		if name[i] == filepath.Separator {
			if _, ok := s[h]; ok {
				return true
			}
		}
		h = (h ^ uint64(name[i])) * fnvPrime
	}
	_, ok := s[h]
	return ok
}

func (s symlinks) add(name string) {
	h := uint64(fnvOffset)
	for i := range len(name) {
		h = (h ^ uint64(name[i])) * fnvPrime
	}
	s[h] = struct{}{}
}

// gunzipIfCompressed returns a reader that decompresses r when it starts with
// the gzip magic bytes, and r's bytes unchanged otherwise.
func gunzipIfCompressed(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}

// entryReader marks errors reading an entry's content as an invalid archive,
// so a truncated or corrupt body is told apart from a failed write.
type entryReader struct {
	tr *tar.Reader
}

func (e entryReader) Read(p []byte) (int, error) {
	n, err := e.tr.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = invalid(err)
	}
	return n, err
}

// invalid marks a read error as an invalid archive, unless MaxTarBytes caused
// it.
func invalid(err error) error {
	if errors.Is(err, ErrTooLarge) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}

type cappedReader struct {
	r    io.Reader
	max  int64
	read int64
	err  error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	// One byte past the cap tells a stream of exactly max bytes from a longer
	// one.
	if left := c.max - c.read; left < int64(len(p)) {
		p = p[:left+1]
	}
	n, err := c.r.Read(p)
	if left := c.max - c.read; int64(n) > left {
		c.read = c.max
		c.err = fmt.Errorf("%w: tar exceeds %d bytes", ErrTooLarge, c.max)
		return int(left), c.err
	}
	c.read += int64(n)
	return n, err
}

// extractor uses names as the archive gives them: cleaning them would change
// what the file system makes of a regular file named "x/".
type extractor struct {
	root *os.Root
}

func (x extractor) mkdir(name string) error {
	return x.root.MkdirAll(name, 0o755)
}

func (x extractor) file(name string, perm os.FileMode, content io.Reader) (int64, error) {
	if err := x.mkdirParent(name); err != nil {
		return 0, err
	}
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

func (x extractor) symlink(target, name string) error {
	if err := x.mkdirParent(name); err != nil {
		return err
	}
	return x.root.Symlink(target, name)
}

func (x extractor) mkdirParent(name string) error {
	if dir := filepath.Dir(name); dir != "." {
		return x.root.MkdirAll(dir, 0o755)
	}
	return nil
}

type validator struct{}

func (validator) mkdir(string) error { return nil }

func (validator) file(_ string, _ os.FileMode, content io.Reader) (int64, error) {
	return io.Copy(io.Discard, content)
}

func (validator) symlink(string, string) error { return nil }
