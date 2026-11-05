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
	"strconv"
	"strings"
	"time"
)

// LocalObjectPath is the API route prefix of the URLs Local.ReadURL returns.
const LocalObjectPath = "/volume-objects/"

// SignatureParam is the query parameter that carries a Local URL's signature.
// It grants access on its own, so it must stay out of logs.
const SignatureParam = "sig"

const (
	expiresParam = "expires"
	tempPrefix   = ".put-"

	// Every file ends with a trailer, so that its content and its digest are
	// committed together by one link: magic, size, sha256.
	trailerMagic = "n8nvol1\x00"
	trailerLen   = len(trailerMagic) + 8 + sha256.Size
)

var (
	errBadSignature     = errors.New("invalid signature")
	errExpiredSignature = errors.New("expired signature")
)

// Local keeps objects as files under one API instance's data directory and
// serves them on that API through URLs signed with a key that is random per
// process. It therefore needs a single API instance, and its URLs stop working
// when the process restarts.
type Local struct {
	root    string
	baseURL string
	key     []byte
	now     func() time.Time
}

// NewLocal stores objects under <dataDir>/volumes. baseURL is where runners
// reach this API; ReadURL returns URLs under it.
func NewLocal(dataDir, baseURL string) (*Local, error) {
	root := filepath.Join(dataDir, "volumes")
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("volumestore: create %s: %w", root, err)
	}
	if info, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("volumestore: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("volumestore: %s is not a directory", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("volumestore: %w", err)
	}
	// No Put runs yet, so every temp file is left over from a crash.
	if err := removeTemps(root); err != nil {
		return nil, fmt.Errorf("volumestore: remove leftover temp files: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Local{
		root:    root,
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		now:     time.Now,
	}, nil
}

func removeTemps(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), tempPrefix) {
			return os.Remove(path)
		}
		return nil
	})
}

func (l *Local) path(key Key) string {
	return filepath.Join(l.root, filepath.FromSlash(key.objectName()))
}

func (l *Local) Put(ctx context.Context, key Key, content io.Reader, opts PutOptions) (Object, error) {
	want, err := checkPut(key, opts)
	if err != nil {
		return Object{}, err
	}
	dst := l.path(key)
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
	if err != nil {
		return Object{}, err
	}
	if size > opts.MaxBytes {
		return Object{}, ErrTooLarge
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
	// Unlike rename, link refuses to replace an existing file.
	if err := os.Link(tmp.Name(), dst); err != nil {
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
	expires := strconv.FormatInt(l.now().Add(ttl).Unix(), 10)
	query := url.Values{
		expiresParam:   {expires},
		SignatureParam: {hex.EncodeToString(l.sign(http.MethodGet, path, expires))},
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
	if err != nil || !hmac.Equal(sig, l.sign(req.Method, req.URL.Path, expires)) {
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

func (l *Local) sign(method, path, expires string) []byte {
	mac := hmac.New(sha256.New, l.key)
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
