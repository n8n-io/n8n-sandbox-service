package volumestore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LocalObjectPath is the route prefix of the URLs Local.ReadURL returns, on
// the API's volume-objects listener.
const LocalObjectPath = "/volume-objects/"

// SignatureParam is the query parameter that carries a Local URL's signature.
// It grants access on its own, so it must stay out of logs.
const SignatureParam = "sig"

// MinSigningKeyBytes is the shortest signing key NewLocal accepts.
const MinSigningKeyBytes = 32

const (
	expiresParam = "expires"
	tempPrefix   = ".put-"

	// NewLocal removes only temp files older than this: another API instance
	// on a shared directory may still be writing a younger one. A day is far
	// longer than any upload and any clock skew to a file server.
	staleTempAge = 24 * time.Hour

	// Every file ends with a trailer, so that its content and its digest are
	// committed together by one link: magic, size, sha256.
	trailerMagic = "n8nvol1\x00"
	trailerLen   = len(trailerMagic) + 8 + sha256.Size
)

var (
	errBadSignature     = errors.New("invalid signature")
	errExpiredSignature = errors.New("expired signature")
)

// Local keeps objects as files in one directory and serves them on the API
// through signed URLs. Several API instances can share it: the directory on a
// shared file system with hard links, such as NFS mounted with
// lookupcache=positive, and the same signing key in every instance. Without a signing key it signs with a key that is random
// per process, so it needs a single API instance, and its URLs stop working
// when the process restarts.
type Local struct {
	// dir is the store's directory, which holds every object.
	dir     string
	baseURL string
	// signingKey signs read URLs; verifyKeys, which start with it, verify
	// them.
	signingKey []byte
	verifyKeys [][]byte
	now        func() time.Time
	link       func(oldname, newname string) error
}

// LocalOptions are the optional settings of NewLocal.
type LocalOptions struct {
	// SigningKey signs and verifies read URLs, at least MinSigningKeyBytes.
	// Empty means a key random per process.
	SigningKey []byte
	// PreviousSigningKey verifies read URLs too but signs none, so that URLs
	// signed before a key rotation keep working. It needs SigningKey.
	PreviousSigningKey []byte
}

// NewLocal stores objects in dir, which it creates if its parent exists.
// baseURL is where runners reach the API's volume-objects listener; ReadURL
// returns URLs under it.
func NewLocal(dir, baseURL string, opts LocalOptions) (*Local, error) {
	verifyKeys, err := checkSigningKeys(opts)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("volumestore: create %s: %w", dir, err)
	}
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("volumestore: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("volumestore: %s is not a directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("volumestore: %w", err)
	}
	if err := checkHardLinks(dir, os.Link); err != nil {
		return nil, err
	}
	if mountinfo, err := os.ReadFile("/proc/self/mountinfo"); err == nil && cachesMissingNames(string(mountinfo), dir) {
		slog.Warn("volumestore: the store directory is on NFS mounted without lookupcache=positive; "+
			"if several API instances share it, objects one of them commits can look missing to another for up to a minute",
			"dir", dir)
	}
	if err := removeStaleTemps(dir, time.Now()); err != nil {
		return nil, fmt.Errorf("volumestore: remove leftover temp files: %w", err)
	}
	if verifyKeys == nil {
		signingKey := make([]byte, MinSigningKeyBytes)
		if _, err := rand.Read(signingKey); err != nil {
			return nil, err
		}
		verifyKeys = [][]byte{signingKey}
	}
	return &Local{
		dir:        dir,
		baseURL:    strings.TrimRight(baseURL, "/"),
		signingKey: verifyKeys[0],
		verifyKeys: verifyKeys,
		now:        time.Now,
		link:       os.Link,
	}, nil
}

// checkSigningKeys returns the keys that verify read URLs, the signing key
// first, or nil for a random one. Errors never show a key.
func checkSigningKeys(opts LocalOptions) ([][]byte, error) {
	if len(opts.SigningKey) == 0 {
		if len(opts.PreviousSigningKey) > 0 {
			return nil, errors.New("volumestore: a previous signing key needs a signing key")
		}
		return nil, nil
	}
	keys := [][]byte{bytes.Clone(opts.SigningKey)}
	if len(opts.PreviousSigningKey) > 0 {
		keys = append(keys, bytes.Clone(opts.PreviousSigningKey))
	}
	for _, key := range keys {
		if len(key) < MinSigningKeyBytes {
			return nil, fmt.Errorf("volumestore: a signing key must be at least %d bytes", MinSigningKeyBytes)
		}
	}
	return keys, nil
}

// checkHardLinks fails unless link makes hard links in dir's file system,
// which Put commits by. SMB shares, for one, have none.
func checkHardLinks(dir string, link func(oldname, newname string) error) error {
	probe, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("volumestore: %w", err)
	}
	defer os.Remove(probe.Name())
	if err := probe.Close(); err != nil {
		return fmt.Errorf("volumestore: %w", err)
	}
	linked := probe.Name() + ".link"
	err = link(probe.Name(), linked)
	defer os.Remove(linked)
	// As in Put, the names decide, not the error.
	if !sameFile(probe.Name(), linked) {
		if err == nil {
			err = errors.New("the link is not the same file")
		}
		return fmt.Errorf("volumestore: %s must be on a file system with hard links, such as a local disk or NFS, not SMB: %w", dir, err)
	}
	return nil
}

// sameFile reports whether both names lead to one file.
func sameFile(name, other string) bool {
	info, err := os.Lstat(name)
	if err != nil {
		return false
	}
	otherInfo, err := os.Lstat(other)
	return err == nil && os.SameFile(info, otherInfo)
}

// cachesMissingNames reports whether dir is on an NFS mount, as listed in
// the text of /proc/self/mountinfo, that remembers names it found missing
// (lookupcache=all, the default). A name another NFS client creates then
// stays missing here until the parent directory's cached attributes expire.
func cachesMissingNames(mountinfo, dir string) bool {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var mountPoint, fsType, superOptions string
	for _, line := range strings.Split(mountinfo, "\n") {
		// ID, parent, device, root, mount point, options, optional fields,
		// "-", file system type, source, super options.
		fields := strings.Fields(line)
		separator := slices.Index(fields, "-")
		if separator < 6 || len(fields) < separator+4 {
			continue
		}
		candidate := unescape.Replace(fields[4])
		within := candidate == "/" || dir == candidate || strings.HasPrefix(dir, candidate+"/")
		// The longest mount point wins, and of equal ones the last mounted.
		if within && len(candidate) >= len(mountPoint) {
			mountPoint, fsType, superOptions = candidate, fields[separator+1], fields[separator+3]
		}
	}
	if fsType != "nfs" && fsType != "nfs4" {
		return false
	}
	for option := range strings.SplitSeq(superOptions, ",") {
		switch option {
		case "lookupcache=pos", "lookupcache=positive", "lookupcache=none":
			return false
		}
	}
	return true
}

// removeStaleTemps removes the temp files under dir that were last written
// before now less staleTempAge. Other API instances may commit or remove
// files meanwhile.
func removeStaleTemps(dir string, now time.Time) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), tempPrefix) {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) < staleTempAge {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

func (l *Local) path(key Key) string {
	return filepath.Join(l.dir, filepath.FromSlash(key.objectName()))
}

func (l *Local) Put(ctx context.Context, key Key, content io.Reader, opts PutOptions) (Object, error) {
	want, err := checkPut(key, opts)
	if err != nil {
		return Object{}, err
	}
	dst := l.path(key)
	// Only an early exit that spares the upload. Two Puts can both pass it;
	// the link below lets only one of them commit.
	if _, err := os.Lstat(dst); err == nil {
		return Object{}, ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Object{}, err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Object{}, err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return Object{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), readUpTo(ctxReader{ctx, content}, opts.MaxBytes))
	if size > opts.MaxBytes {
		return Object{}, ErrTooLarge
	}
	if err != nil {
		return Object{}, err
	}
	sum := hasher.Sum(nil)
	if want != nil && !bytes.Equal(sum, want) {
		return Object{}, ErrDigestMismatch
	}
	if _, err := tmp.Write(trailer(size, sum)); err != nil {
		return Object{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Object{}, err
	}
	if err := tmp.Close(); err != nil {
		return Object{}, err
	}
	// Unlike rename, link refuses to replace an existing file. Over NFS a
	// link that was made can still fail, as EEXIST for its own retried
	// request, so the name decides, as open(2) advises for lock files.
	if err := l.link(tmp.Name(), dst); err != nil && !sameFile(tmp.Name(), dst) {
		if errors.Is(err, fs.ErrExist) {
			return Object{}, ErrExists
		}
		return Object{}, err
	}
	// The object is committed; a failure here only weakens durability.
	if err := syncDir(dir); err != nil {
		slog.Warn("volumestore: sync directory", "dir", dir, "error", err)
	}
	return Object{Size: size, SHA256: hex.EncodeToString(sum)}, nil
}

// readUpTo lets reader read one byte past limit, so going past it shows.
func readUpTo(reader io.Reader, limit int64) io.Reader {
	if limit < math.MaxInt64 {
		limit++
	}
	return io.LimitReader(reader, limit)
}

func trailer(size int64, sum []byte) []byte {
	buf := make([]byte, 0, trailerLen)
	buf = append(buf, trailerMagic...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(size))
	return append(buf, sum...)
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

type localFile struct {
	file    *os.File
	obj     Object
	modTime time.Time
}

func (l *Local) open(key Key) (*localFile, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	file, err := os.Open(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	tail := make([]byte, trailerLen)
	if info.Size() < int64(trailerLen) {
		file.Close()
		return nil, fmt.Errorf("volumestore: %s is truncated", file.Name())
	}
	if _, err := file.ReadAt(tail, info.Size()-int64(trailerLen)); err != nil {
		file.Close()
		return nil, err
	}
	size := int64(binary.BigEndian.Uint64(tail[len(trailerMagic):]))
	if string(tail[:len(trailerMagic)]) != trailerMagic || size != info.Size()-int64(trailerLen) {
		file.Close()
		return nil, fmt.Errorf("volumestore: %s has no valid trailer", file.Name())
	}
	return &localFile{
		file:    file,
		obj:     Object{Size: size, SHA256: hex.EncodeToString(tail[len(trailerMagic)+8:])},
		modTime: info.ModTime(),
	}, nil
}

// Stat reads the size and digest from the file's trailer, which anyone who
// can write the store's directory controls.
func (l *Local) Stat(_ context.Context, key Key) (Object, error) {
	lf, err := l.open(key)
	if err != nil {
		return Object{}, err
	}
	lf.file.Close()
	return lf.obj, nil
}

func (l *Local) Open(_ context.Context, key Key) (io.ReadCloser, Object, error) {
	lf, err := l.open(key)
	if err != nil {
		return nil, Object{}, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(lf.file, 0, lf.obj.Size), lf.file}, lf.obj, nil
}

// Delete removes the object. On NFS that also ends the downloads of it that
// other API instances are serving: their reads fail with ESTALE.
func (l *Local) Delete(_ context.Context, key Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := os.Remove(l.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReadURL returns <baseURL>/volume-objects/<object name>?expires=…&sig=…, an
// HMAC-SHA256 over method, path and expiry that VerifyRequest checks.
func (l *Local) ReadURL(_ context.Context, key Key, ttl time.Duration) (string, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	ttl, err := checkTTL(ttl)
	if err != nil {
		return "", err
	}
	path := LocalObjectPath + key.objectName()
	// Rounded up, so the URL lasts at least ttl.
	expiry := l.now().Add(ttl)
	expiryUnix := expiry.Unix()
	if expiry.Nanosecond() > 0 {
		expiryUnix++
	}
	expires := strconv.FormatInt(expiryUnix, 10)
	query := url.Values{
		expiresParam:   {expires},
		SignatureParam: {hex.EncodeToString(sign(l.signingKey, http.MethodGet, path, expires))},
	}
	return l.baseURL + path + "?" + query.Encode(), nil
}

// VerifyRequest checks that req carries an unexpired signature from ReadURL
// for its method and path, and returns the key the path names.
func (l *Local) VerifyRequest(req *http.Request) (Key, error) {
	name, ok := strings.CutPrefix(req.URL.Path, LocalObjectPath)
	if !ok {
		return Key{}, errBadSignature
	}
	// Parsed first, so the signed string can never contain a separator.
	key, err := parseObjectName(name)
	if err != nil {
		return Key{}, errBadSignature
	}
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil || len(query[expiresParam]) != 1 || len(query[SignatureParam]) != 1 {
		return Key{}, errBadSignature
	}
	expires := query.Get(expiresParam)
	sig, err := hex.DecodeString(query.Get(SignatureParam))
	if err != nil || !l.validSignature(sig, req.Method, req.URL.Path, expires) {
		return Key{}, errBadSignature
	}
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return Key{}, errBadSignature
	}
	if !l.now().Before(time.Unix(exp, 0)) {
		return Key{}, errExpiredSignature
	}
	return key, nil
}

func (l *Local) validSignature(sig []byte, method, path, expires string) bool {
	for _, key := range l.verifyKeys {
		if hmac.Equal(sig, sign(key, method, path, expires)) {
			return true
		}
	}
	return false
}

func sign(key []byte, method, path, expires string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(method + "\n" + path + "\n" + expires))
	return mac.Sum(nil)
}

// ServeObject writes the object with http.ServeContent, so range and
// conditional requests work. It returns an error, ErrNotFound for a missing
// object, only before writing anything.
func (l *Local) ServeObject(writer http.ResponseWriter, req *http.Request, key Key) error {
	lf, err := l.open(key)
	if err != nil {
		return err
	}
	defer lf.file.Close()
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	// A cached copy would outlive the URL's expiry.
	writer.Header().Set("Cache-Control", "no-store")
	http.ServeContent(writer, req, "", lf.modTime, io.NewSectionReader(lf.file, 0, lf.obj.Size))
	return nil
}
