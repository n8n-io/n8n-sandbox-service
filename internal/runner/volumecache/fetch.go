package volumecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second

	// sparseBlock is the granularity at which all-zero data is left as a hole,
	// the block size of ext4 and XFS.
	sparseBlock = 4 << 10
	copyBuffer  = 1 << 20
)

var zeroBlock [sparseBlock]byte

// newTransport is used without an http.Client, so a redirect is just a 3xx
// status: the API hands out a URL to the object itself, and following one would
// let the storage point the runner anywhere it can reach. A client would also
// parse the Location header first and quote it, token included, in its error.
// A nil tlsConfig means the system's CAs and no client certificate; a nil proxy
// means none, whatever the environment says.
func newTransport(tlsConfig *tls.Config, proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       tlsConfig,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		// The digest covers the bytes as stored, not a decoded form of them.
		DisableCompression: true,
	}
}

func parseSource(src Source) (*url.URL, error) {
	if src.Size <= 0 {
		return nil, fmt.Errorf("volume object size must be positive, got %d", src.Size)
	}
	parsed, err := url.Parse(src.URL)
	if err != nil {
		// Not wrapped: url.Parse quotes the whole URL, token included.
		return nil, errors.New("invalid volume object URL")
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("volume object URL scheme %q is not https", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("volume object URL has no host")
	}
	return parsed, nil
}

// transportFor picks the registration identity for a URL on the API's host,
// whatever its port, and the plain transport for every other URL.
func (c *Cache) transportFor(sourceURL *url.URL) *http.Transport {
	if c.apiTransport != nil && sameHostName(sourceURL.Hostname(), c.config.APIHost) {
		return c.apiTransport
	}
	return c.transport
}

// sameHostName compares host names ignoring ASCII case only. strings.EqualFold
// also folds Unicode, so "ſandbox-api" (a long s) would match "sandbox-api".
func sameHostName(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range len(left) {
		if lowerASCII(left[index]) != lowerASCII(right[index]) {
			return false
		}
	}
	return true
}

func lowerASCII(char byte) byte {
	if 'A' <= char && char <= 'Z' {
		return char + 'a' - 'A'
	}
	return char
}

// redact is the form of a source URL fit for logs: queries carry SAS or HMAC
// tokens.
func redact(sourceURL *url.URL) string {
	return (&url.URL{Scheme: sourceURL.Scheme, Host: sourceURL.Host, Path: sourceURL.Path}).String()
}

// fetchError is a failed download with its fetches_total result. Its message
// names neither the URL nor a host path or address, since it can reach the
// client whose create failed; cause, which can, is only logged.
type fetchError struct {
	result string
	err    error
	cause  error
}

func (e *fetchError) Error() string { return "download volume object: " + e.err.Error() }

func (e *fetchError) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return e.err
}

func resultOf(err error) string {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.result
	}
	return metrics.VolumeFetchIO
}

func causeOf(err error) error {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.cause
	}
	return nil
}

func failed(result string, format string, args ...any) error {
	return &fetchError{result: result, err: fmt.Errorf(format, args...)}
}

// diskFailure reports a disk that filled up during a download as a full cache:
// the check before it passed, and nothing has been created for the sandbox.
func diskFailure(op string, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return &fetchError{result: metrics.VolumeFetchFull, err: fmt.Errorf("%s: no space left on device: %w", op, ErrCacheFull)}
	}
	return &fetchError{result: metrics.VolumeFetchIO, err: fmt.Errorf("%s: %w", op, withoutPath(err))}
}

func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// networkFailure classifies an error from the request or the body. ctx only
// ends at the fetch timeout, so any error after that is a timeout.
func networkFailure(ctx context.Context, err error) error {
	var ne net.Error
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &fetchError{result: metrics.VolumeFetchTimeout, err: errors.New("timed out"), cause: err}
	}
	return &fetchError{result: metrics.VolumeFetchHTTP, err: errors.New("request failed"), cause: err}
}

// download fetches sourceURL into dest, refusing anything but exactly size
// bytes with the given sha256, and returns the allocated bytes of the file.
// dest is only ever written by renaming a verified, read-only file onto it.
func (c *Cache) download(ctx context.Context, sourceURL *url.URL, size int64, digest, dest string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL.String(), nil)
	if err != nil {
		return 0, failed(metrics.VolumeFetchHTTP, "build request")
	}
	resp, err := c.transportFor(sourceURL).RoundTrip(req)
	if err != nil {
		return 0, networkFailure(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, failed(metrics.VolumeFetchHTTP, "status %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return 0, failed(metrics.VolumeFetchSize, "content length %d, want %d", resp.ContentLength, size)
	}

	tmp, err := os.CreateTemp(c.config.Dir, ".download-*")
	if err != nil {
		return 0, diskFailure("create file", err)
	}
	renamed := false
	defer func() {
		_ = tmp.Close()
		// After the rename the name is free again for another download's
		// temp file.
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()

	sum := sha256.New()
	written, err := copySparse(tmp, io.LimitReader(resp.Body, size+1), sum, bufferSize(size))
	if err != nil {
		var fe *fetchError
		if !errors.As(err, &fe) {
			err = networkFailure(ctx, err)
		}
		return 0, err
	}
	if written != size {
		return 0, failed(metrics.VolumeFetchSize, "body is %s than the recorded %d bytes", longerOrShorter(written, size), size)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != digest {
		return 0, failed(metrics.VolumeFetchDigest, "sha256 %s, want %s", got, digest)
	}
	// Extends the file over trailing zeros, which were never written.
	if err := tmp.Truncate(size); err != nil {
		return 0, diskFailure("truncate file", err)
	}
	if err := tmp.Chmod(0o444); err != nil {
		return 0, diskFailure("chmod file", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, diskFailure("close file", err)
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		return 0, diskFailure("stat file", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return 0, diskFailure("rename file", err)
	}
	renamed = true
	return allocatedBytes(info), nil
}

func longerOrShorter(got, size int64) string {
	if got > size {
		return "longer"
	}
	return "shorter"
}

// bufferSize fits size+1 bytes, the most copySparse reads, in whole blocks.
func bufferSize(size int64) int {
	return int(min(copyBuffer, (size+sparseBlock)/sparseBlock*sparseBlock))
}

func allocatedBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}

// copySparse writes src to dst from offset 0 and hashes every byte, skipping
// all-zero blocks so they stay holes. bufSize is a multiple of sparseBlock.
// Write failures come back as fetchErrors, read failures as they are.
func copySparse(dst *os.File, src io.Reader, sum hash.Hash, bufSize int) (int64, error) {
	buf := make([]byte, bufSize)
	var off int64
	for {
		chunkLen, rerr := io.ReadFull(src, buf)
		if chunkLen > 0 {
			sum.Write(buf[:chunkLen])
			if err := writeNonZero(dst, buf[:chunkLen], off); err != nil {
				return off, diskFailure("write file", err)
			}
			off += int64(chunkLen)
		}
		// A body cut off mid-transfer ends here too; the size check refuses it.
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return off, nil
		}
		if rerr != nil {
			return off, rerr
		}
	}
}

// writeNonZero writes data at off, one write per run of blocks that are not
// all zero. off is a multiple of sparseBlock.
func writeNonZero(dst *os.File, data []byte, off int64) error {
	isZero := func(start int) (bool, int) {
		end := min(start+sparseBlock, len(data))
		return bytes.Equal(data[start:end], zeroBlock[:end-start]), end
	}
	for start := 0; start < len(data); {
		zero, end := isZero(start)
		if zero {
			start = end
			continue
		}
		for end < len(data) {
			nextZero, next := isZero(end)
			if nextZero {
				break
			}
			end = next
		}
		if _, err := dst.WriteAt(data[start:end], off+int64(start)); err != nil {
			return err
		}
		start = end
	}
	return nil
}
