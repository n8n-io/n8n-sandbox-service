// Package archivestore keeps sealed sandbox archives. Runners never hold its
// credentials: the API hands them a URL signed for one object and one method,
// valid for a short while.
package archivestore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Store is where archives live.
type Store interface {
	// UploadURL and DownloadURL sign a PUT or GET of object.
	UploadURL(object string) (string, error)
	DownloadURL(object string) (string, error)
	// Size is the stored object's size; ErrNotFound when there is none.
	Size(ctx context.Context, object string) (int64, error)
	Delete(ctx context.Context, object string) error
}

// ErrNotFound is an object the store does not have.
var ErrNotFound = errors.New("archive object not found")

// urlLifetime bounds a signed URL: long enough to move a large disk.
const urlLifetime = time.Hour

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidObject reports whether object is a name the API makes:
// <tenant>/<sandbox>/<archive>, each a plain segment.
func ValidObject(object string) bool {
	parts := strings.Split(object, "/")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !segmentRe.MatchString(p) {
			return false
		}
	}
	return true
}

// Dir keeps archives in a directory and serves them to runners at
// <baseURL>/internal/archives/<object>, for a single API or one with shared
// storage.
type Dir struct {
	root    *os.Root
	baseURL string
	key     []byte
	now     func() time.Time
}

// HandlerPrefix is the path the Dir handler serves.
const HandlerPrefix = "/internal/archives/"

// NewDir serves dir at baseURL (the API as runners reach it), signing with key.
func NewDir(dir, baseURL string, key []byte) (*Dir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("archive URL %q must be an absolute http(s) URL", baseURL)
	}
	return &Dir{root: root, baseURL: strings.TrimRight(baseURL, "/"), key: key, now: time.Now}, nil
}

func (d *Dir) sign(method, object string, exp int64) string {
	m := hmac.New(sha256.New, d.key)
	fmt.Fprintf(m, "%s\n%s\n%d", method, object, exp)
	return hex.EncodeToString(m.Sum(nil))
}

func (d *Dir) signedURL(method, object string) (string, error) {
	if !ValidObject(object) {
		return "", fmt.Errorf("invalid archive object %q", object)
	}
	exp := d.now().Add(urlLifetime).Unix()
	return fmt.Sprintf("%s%s%s?exp=%d&sig=%s", d.baseURL, HandlerPrefix, object, exp, d.sign(method, object, exp)), nil
}

func (d *Dir) UploadURL(object string) (string, error)   { return d.signedURL(http.MethodPut, object) }
func (d *Dir) DownloadURL(object string) (string, error) { return d.signedURL(http.MethodGet, object) }

func (d *Dir) Size(_ context.Context, object string) (int64, error) {
	if !ValidObject(object) {
		return 0, ErrNotFound
	}
	info, err := d.root.Stat(object)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d *Dir) Delete(_ context.Context, object string) error {
	if !ValidObject(object) {
		return nil
	}
	if err := d.root.Remove(object); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ServeHTTP serves signed PUTs and GETs; anything else, an expired URL or a
// bad signature is refused with no detail.
func (d *Dir) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	object := strings.TrimPrefix(r.URL.Path, HandlerPrefix)
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if !ValidObject(object) || err != nil || d.now().Unix() > exp || (r.Method != http.MethodPut && r.Method != http.MethodGet) ||
		!hmac.Equal([]byte(d.sign(r.Method, object, exp)), []byte(r.URL.Query().Get("sig"))) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		err := d.put(object, r.Body)
		if errors.Is(err, os.ErrExist) {
			http.Error(w, "object exists", http.StatusPreconditionFailed)
			return
		}
		if err != nil {
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		f, err := d.root.Open(object)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		if info, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	}
}

// put writes to a temporary file and links it into place, so a reader never
// sees half an object and an object, once written, is never replaced: a
// signed upload URL writes once.
func (d *Dir) put(object string, body io.Reader) error {
	if _, err := d.root.Stat(object); err == nil {
		return os.ErrExist
	}
	dir := path.Dir(object)
	if err := d.root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := object + ".tmp-" + strconv.FormatInt(d.now().UnixNano(), 36)
	f, err := d.root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = d.root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = d.root.Remove(tmp)
		return err
	}
	defer func() { _ = d.root.Remove(tmp) }()
	return d.root.Link(tmp, object)
}
