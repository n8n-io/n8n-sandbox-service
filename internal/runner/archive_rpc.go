package runner

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/archive"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// archiveClient moves archives; never follows a redirect, which would carry a
// signed URL's object elsewhere.
var archiveClient = &http.Client{CheckRedirect: runnerruntime.RefuseRedirect}

func validArchiveURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("archive URL must be an absolute http(s) URL")
	}
	return nil
}

// ArchiveSandbox exports a stopped sandbox's disk, compresses and seals it
// with the archive's key, and uploads it to the signed URL. The sandbox stays
// until the API, having recorded the archive, deletes it.
func (s *SandboxControlGRPC) ArchiveSandbox(ctx context.Context, req *pb.ArchiveSandboxRequest) (*pb.ArchiveSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	archiver, ok := s.Runtime.(runnerruntime.Archiver)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this runner cannot archive")
	}
	if err := validArchiveURL(req.GetUploadUrl()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.Activity.Active(sandboxID) {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is active")
	}

	pr, pw := io.Pipe()
	hash := sha256.New()
	counter := &countingWriter{}
	go func() {
		sealer, err := archive.NewWriter(io.MultiWriter(pw, hash, counter), req.GetKey())
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		zw := gzip.NewWriter(sealer)
		if err := archiver.ExportSandbox(ctx, sandboxID, zw); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(sealer.Close())
	}()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, req.GetUploadUrl(), pr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	// An upload URL writes one object once; the store refuses a second PUT.
	httpReq.Header.Set("If-None-Match", "*")
	resp, err := archiveClient.Do(httpReq)
	if err != nil {
		_ = pr.CloseWithError(err)
		if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			return nil, status.Error(codes.NotFound, "sandbox not found")
		}
		return nil, status.Error(codes.Unavailable, "upload archive: "+withoutURL(err).Error())
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, status.Errorf(codes.Unavailable, "archive store answered %d", resp.StatusCode)
	}
	return &pb.ArchiveSandboxResponse{Size: counter.n, Sha256: hex.EncodeToString(hash.Sum(nil))}, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }

// RestoreSandbox creates a sandbox from an archive with the checks and egress
// setup of CreateSandbox. An archive that does not open fails with DataLoss.
func (s *SandboxControlGRPC) RestoreSandbox(ctx context.Context, req *pb.RestoreSandboxRequest) (*pb.CreateSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	if err := validArchiveURL(req.GetDownloadUrl()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	key := req.GetKey()
	open := func(ctx context.Context) (io.ReadCloser, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.GetDownloadUrl(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := archiveClient.Do(httpReq)
		if err != nil {
			return nil, withoutURL(err)
		}
		// A missing object is not taken as lost here: the API checks its store
		// before a restore, and a 404 may be a store that is not ready.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("archive store answered %d", resp.StatusCode)
		}
		opened, err := archive.NewReader(cutShort{resp.Body}, key)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		zr, err := gzip.NewReader(opened)
		if err != nil {
			resp.Body.Close()
			if corrupt(err) {
				return nil, errArchiveLost
			}
			return nil, err
		}
		return readCloser{Reader: &lostOnCorrupt{r: zr}, Closer: resp.Body}, nil
	}
	return s.create(ctx, req.GetSandboxId(), req.GetCreateOptionsJson(), open)
}

var errArchiveLost = errors.New("archive does not open")

// corrupt reports whether err says the archive itself is bad, as opposed to
// a failed read: it fails authentication, or what it seals is not gzip.
func corrupt(err error) bool {
	var flateErr flate.CorruptInputError
	return errors.Is(err, archive.ErrCorrupt) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &flateErr)
}

// cutShort keeps a download that ends early apart from an archive that does:
// the body reports io.ErrUnexpectedEOF when the connection drops before its
// Content-Length.
type cutShort struct{ r io.Reader }

var errDownloadCut = errors.New("archive download cut short")

func (c cutShort) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = errDownloadCut
	}
	return n, err
}

// withoutURL drops the signed URL a transport error carries.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s archive: %w", ue.Op, ue.Err)
	}
	return err
}

// lostOnCorrupt turns an archive that fails authentication into errArchiveLost,
// and remembers it: a runtime that reads it through a subprocess reports the
// failure in words of its own. Any other read error passes through.
type lostOnCorrupt struct {
	r    io.Reader
	lost bool
}

func (l *lostOnCorrupt) Read(b []byte) (int, error) {
	n, err := l.r.Read(b)
	if err != nil && err != io.EOF && corrupt(err) {
		l.lost = true
		return n, errArchiveLost
	}
	return n, err
}

type readCloser struct {
	io.Reader
	io.Closer
}

// restoreFrom opens the archive and imports it. The restore runs under its
// own budget: a large disk takes longer than a create.
func restoreFrom(ctx context.Context, a runnerruntime.Archiver, sandboxID string, opts *runnerruntime.CreateOptions, archive func(context.Context) (io.ReadCloser, error)) (*runnerruntime.SandboxInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, restoreBudget)
	defer cancel()
	r, err := archive(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := a.ImportSandbox(ctx, sandboxID, opts, r)
	if rc, ok := r.(readCloser); ok {
		if l, ok := rc.Reader.(*lostOnCorrupt); ok && l.lost && err != nil {
			return nil, errArchiveLost
		}
	}
	return info, err
}

const restoreBudget = 30 * time.Minute
