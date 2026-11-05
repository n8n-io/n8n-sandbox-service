package volumestore

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// remoteBase addresses the helper; the transport dials its socket whatever
// the host.
const remoteBase = "http://volume-store/v1"

// remoteIdleTimeout fails a call once nothing has moved on its connection,
// either way, for this long, so a stuck helper cannot hold it.
var remoteIdleTimeout = time.Minute

// Remote keeps objects through a store helper, a process next to the API
// that holds the storage credentials and serves the protocol in
// docs/volume-store-helper.md on a Unix socket.
type Remote struct {
	client *http.Client
}

// NewRemote talks to the helper listening on socket. It does not connect yet.
func NewRemote(socket string) *Remote {
	var dialer net.Dialer
	idle := remoteIdleTimeout
	return &Remote{client: &http.Client{
		// No Proxy: the socket is the only destination.
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, "unix", socket)
				if err != nil {
					return nil, err
				}
				return idleConn{Conn: conn, timeout: idle}, nil
			},
			ResponseHeaderTimeout:  time.Minute,
			MaxResponseHeaderBytes: 64 << 10,
			// Before idleConn would fail a pooled connection mid-request.
			IdleConnTimeout: idle / 2,
			// Transparent gzip would drop Content-Length, which carries the size.
			DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

// Read and Write move both deadlines, so a long upload keeps the pending
// read for the answer alive.
func (c idleConn) Read(buf []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(buf)
}

func (c idleConn) Write(buf []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(buf)
}

func objectURL(key Key) string {
	return remoteBase + "/objects/" + key.objectName()
}

// Put counts and hashes the upload itself and cuts it off before its end when
// it grows past MaxBytes or ends with the wrong digest, so the helper, which
// stores only a complete body, never stores it. If the helper answers before
// the upload ends, one read of content may still be under way when Put
// returns.
func (rs *Remote) Put(ctx context.Context, key Key, content io.Reader, opts PutOptions) (Object, error) {
	want, err := checkPut(key, opts)
	if err != nil {
		return Object{}, err
	}
	body := &putBody{source: readUpTo(ctxReader{ctx, content}, opts.MaxBytes), limit: opts.MaxBytes, want: want, hasher: sha256.New()}
	query := url.Values{"max_bytes": {strconv.FormatInt(opts.MaxBytes, 10)}}
	if want != nil {
		query.Set("sha256", hex.EncodeToString(want))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, objectURL(key)+"?"+query.Encode(), body)
	if err != nil {
		return Object{}, err
	}
	resp, err := rs.client.Do(req)
	sent, complete, berr := body.finish()
	if resp != nil {
		defer resp.Body.Close()
	}
	switch {
	case resp != nil && resp.StatusCode == http.StatusCreated && (berr != nil || !complete):
		return Object{}, rs.discard(ctx, key, cmp.Or(berr, errors.New("volumestore: helper stored the object before the upload ended")))
	case berr != nil:
		return Object{}, berr
	case err != nil && complete:
		return rs.settle(ctx, key, sent, fmt.Errorf("volumestore: helper: %w", err))
	case err != nil:
		return Object{}, fmt.Errorf("volumestore: helper: %w", err)
	case resp.StatusCode >= 500 && complete:
		return rs.settle(ctx, key, sent, helperError(resp))
	case resp.StatusCode != http.StatusCreated:
		return Object{}, helperError(resp)
	}
	var got struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&got); err != nil {
		cause := fmt.Errorf("volumestore: helper answer: %w", err)
		obj, err := rs.settle(ctx, key, sent, cause)
		if errors.Is(err, ErrExists) {
			// The helper said it created what is there.
			return Object{}, rs.discard(ctx, key, cause)
		}
		return obj, err
	}
	if got.Size != sent.Size || !strings.EqualFold(got.SHA256, sent.SHA256) {
		return Object{}, rs.discard(ctx, key, fmt.Errorf("volumestore: helper stored %d bytes with sha256 %.64q, not what was sent", got.Size, got.SHA256))
	}
	return sent, nil
}

// settle decides a Put whose whole upload went out but whose answer says
// nothing about whether it was stored. An object with exactly the sent
// content counts as stored.
func (rs *Remote) settle(ctx context.Context, key Key, sent Object, cause error) (Object, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	got, err := rs.Stat(ctx, key)
	switch {
	case err == nil && got == sent:
		return sent, nil
	case err == nil:
		return Object{}, ErrExists
	case errors.Is(err, ErrNotFound):
		return Object{}, cause
	}
	return Object{}, fmt.Errorf("%w; whether it was stored is unknown: %v", cause, err)
}

// discard deletes an object the helper stored although it must not have.
func (rs *Remote) discard(ctx context.Context, key Key, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := rs.Delete(ctx, key); err != nil {
		return fmt.Errorf("%w; deleting what it stored failed: %v", cause, err)
	}
	return cause
}

// putBody is the upload as the transport reads it. The transport may go on
// reading after Do returns; once finished, reads no longer reach the caller's
// reader.
type putBody struct {
	source io.Reader
	limit  int64
	want   []byte

	mu       sync.Mutex
	hasher   hash.Hash
	sent     int64
	complete bool
	err      error
}

var errPutFinished = errors.New("volumestore: upload finished")

func (b *putBody) Read(buf []byte) (int, error) {
	b.mu.Lock()
	err := b.err
	b.mu.Unlock()
	if err != nil {
		return 0, err
	}
	read, err := b.source.Read(buf)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	b.sent += int64(read)
	if b.sent > b.limit {
		b.err = ErrTooLarge
		return 0, b.err
	}
	b.hasher.Write(buf[:read])
	switch {
	case errors.Is(err, io.EOF):
		if b.want != nil && !bytes.Equal(b.hasher.Sum(nil), b.want) {
			b.err = ErrDigestMismatch
			return 0, b.err
		}
		b.complete = true
	case err != nil:
		b.err = err
	}
	return read, err
}

// finish returns what was read, whether all of it went out, and why the
// upload was cut off, if it was.
func (b *putBody) finish() (Object, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.err
	if b.err == nil {
		b.err = errPutFinished
	}
	return Object{Size: b.sent, SHA256: hex.EncodeToString(b.hasher.Sum(nil))}, b.complete, err
}

func (rs *Remote) Stat(ctx context.Context, key Key) (Object, error) {
	resp, err := rs.do(ctx, http.MethodHead, key, http.StatusOK)
	if err != nil {
		return Object{}, err
	}
	resp.Body.Close()
	return objectHeaders(resp)
}

func (rs *Remote) Open(ctx context.Context, key Key) (io.ReadCloser, Object, error) {
	resp, err := rs.do(ctx, http.MethodGet, key, http.StatusOK)
	if err != nil {
		return nil, Object{}, err
	}
	obj, err := objectHeaders(resp)
	if err != nil {
		resp.Body.Close()
		return nil, Object{}, err
	}
	return resp.Body, obj, nil
}

func (rs *Remote) Delete(ctx context.Context, key Key) error {
	resp, err := rs.do(ctx, http.MethodDelete, key, http.StatusNoContent, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// do sends a bodiless request for key and returns the response if its status
// is one of ok.
func (rs *Remote) do(ctx context.Context, method string, key Key, ok ...int) (*http.Response, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := rs.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("volumestore: helper: %w", err)
	}
	if !slices.Contains(ok, resp.StatusCode) {
		defer resp.Body.Close()
		return nil, helperError(resp)
	}
	return resp, nil
}

func (rs *Remote) ReadURL(ctx context.Context, key Key, ttl time.Duration) (string, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	ttl, err := checkTTL(ttl)
	if err != nil {
		return "", err
	}
	reqBody, err := json.Marshal(map[string]any{
		"name":        key.objectName(),
		"ttl_seconds": ceilSeconds(ttl),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, remoteBase+"/read-urls", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rs.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("volumestore: helper: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", helperError(resp)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("volumestore: helper answer: %w", err)
	}
	if parsed, err := url.Parse(out.URL); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("volumestore: helper returned a read URL that is not a plain https URL")
	}
	return out.URL, nil
}

func objectHeaders(resp *http.Response) (Object, error) {
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return Object{}, errors.New("volumestore: helper sent no valid Content-Length")
	}
	sum, err := hex.DecodeString(resp.Header.Get("Object-SHA256"))
	if err != nil || len(sum) != sha256.Size {
		return Object{}, errors.New("volumestore: helper sent no valid Object-SHA256")
	}
	return Object{Size: size, SHA256: hex.EncodeToString(sum)}, nil
}

// helperError maps the helper's status codes onto the Store errors.
func helperError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrExists
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusUnprocessableEntity:
		return ErrDigestMismatch
	}
	var body struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if json.Unmarshal(raw, &body) != nil || body.Error == "" {
		body.Error = strings.TrimSpace(string(raw))
	}
	if body.Error == "" {
		return fmt.Errorf("volumestore: helper answered %d", resp.StatusCode)
	}
	return fmt.Errorf("volumestore: helper answered %d: %.512s", resp.StatusCode, body.Error)
}
