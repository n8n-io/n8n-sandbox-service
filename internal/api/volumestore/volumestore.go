// Package volumestore stores the objects behind read-only sandbox volumes: an
// ext4 image for Firecracker runners, a tar for Docker runners. Runners read an
// object through a short-lived URL from ReadURL and never hold storage
// credentials.
package volumestore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

var (
	ErrNotFound       = errors.New("volume object not found")
	ErrExists         = errors.New("volume object already exists")
	ErrTooLarge       = errors.New("volume object larger than allowed")
	ErrDigestMismatch = errors.New("volume object sha256 does not match")
	ErrInvalidKey     = errors.New("invalid volume object key")
)

// MaxReadURLTTL caps the lifetime of a URL from ReadURL.
const MaxReadURLTTL = time.Hour

var objectIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Key names one object. Scope is volume.SharedScope or a tenant ID; ID is
// chosen by the API.
type Key struct {
	Scope string
	ID    string
	Kind  volume.Kind
}

// Validate reports whether every field of k is well-formed, so that the
// object name derived from it cannot leave its scope.
func (k Key) Validate() error {
	if !volume.ValidScope(k.Scope) {
		return fmt.Errorf("%w: scope must be %q or a tenant ID", ErrInvalidKey, volume.SharedScope)
	}
	if !objectIDPattern.MatchString(k.ID) {
		return fmt.Errorf("%w: id must be 1 to 64 characters of a-z, 0-9 and -", ErrInvalidKey)
	}
	if !k.Kind.Valid() {
		return fmt.Errorf("%w: kind must be %q or %q", ErrInvalidKey, volume.KindExt4, volume.KindTar)
	}
	return nil
}

// objectName is the slash-separated name both backends store k under. k must
// be valid.
func (k Key) objectName() string {
	file := k.ID + "." + string(k.Kind)
	if k.Scope == volume.SharedScope {
		return volume.SharedScope + "/" + file
	}
	return "tenants/" + k.Scope + "/" + file
}

// parseObjectName is the inverse of objectName and accepts nothing else.
func parseObjectName(name string) (Key, error) {
	var key Key
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 2 && parts[0] == volume.SharedScope:
		key.Scope = volume.SharedScope
	case len(parts) == 3 && parts[0] == "tenants":
		key.Scope = parts[1]
	default:
		return Key{}, ErrInvalidKey
	}
	id, kind, ok := strings.Cut(parts[len(parts)-1], ".")
	if !ok {
		return Key{}, ErrInvalidKey
	}
	key.ID, key.Kind = id, volume.Kind(kind)
	if err := key.Validate(); err != nil {
		return Key{}, err
	}
	if key.objectName() != name {
		return Key{}, ErrInvalidKey
	}
	return key, nil
}

// Object describes a stored object. SHA256 is lowercase hex.
type Object struct {
	Size   int64
	SHA256 string
}

// PutOptions bound what Put accepts. MaxBytes is required; ExpectedSHA256 is
// optional lowercase hex.
type PutOptions struct {
	MaxBytes       int64
	ExpectedSHA256 string
}

// Store keeps volume objects. Objects are immutable: Put never replaces one.
type Store interface {
	// Put streams content into a new object, hashing it on the way. It returns
	// ErrTooLarge past MaxBytes, ErrDigestMismatch when ExpectedSHA256 is set
	// and differs, and ErrExists when the object already exists. Nothing is
	// visible after an error, unless a remote store's answer got lost after
	// the whole upload went out.
	Put(ctx context.Context, key Key, content io.Reader, opts PutOptions) (Object, error)
	// Stat returns ErrNotFound when the object does not exist.
	Stat(ctx context.Context, key Key) (Object, error)
	// Open returns the object's content; the caller closes it.
	Open(ctx context.Context, key Key) (io.ReadCloser, Object, error)
	// Delete removes the object; a missing object is not an error.
	Delete(ctx context.Context, key Key) error
	// ReadURL returns a URL that allows reading this one object, and nothing
	// else, for ttl, capped at MaxReadURLTTL. It does not check that the
	// object exists.
	ReadURL(ctx context.Context, key Key, ttl time.Duration) (string, error)
}

// checkPut validates the arguments of a Put and returns the expected digest,
// nil when none was given.
func checkPut(key Key, opts PutOptions) ([]byte, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if opts.MaxBytes <= 0 {
		return nil, errors.New("volumestore: MaxBytes must be positive")
	}
	if opts.ExpectedSHA256 == "" {
		return nil, nil
	}
	if !volume.ValidDigest(opts.ExpectedSHA256) {
		return nil, errors.New("volumestore: ExpectedSHA256 must be a sha256 in lowercase hex")
	}
	return hex.DecodeString(opts.ExpectedSHA256)
}

func checkTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 {
		return 0, errors.New("volumestore: ttl must be positive")
	}
	return min(ttl, MaxReadURLTTL), nil
}

// ceilSeconds rounds up, so a URL with whole-second expiry lasts at least
// the ttl it was asked for.
func ceilSeconds(duration time.Duration) int64 {
	return int64((duration + time.Second - 1) / time.Second)
}

// ctxReader stops a long copy once ctx is done.
type ctxReader struct {
	ctx    context.Context
	reader io.Reader
}

func (c ctxReader) Read(buf []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.reader.Read(buf)
}
