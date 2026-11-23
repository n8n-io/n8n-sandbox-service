package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// sharedVolumesPath is the root of the shared volume routes, the only ones a
// volume publisher key reaches.
const sharedVolumesPath = "/shared-volumes"

// Reasons of the shared volume routes' errors.
const (
	reasonVolumeStoreDisabled = "volume_store_disabled"
	reasonNameTaken           = "name_taken"
	reasonNameMismatch        = "name_mismatch"
	reasonPublishInProgress   = "publish_in_progress"
	reasonSharedVolumeLimit   = "shared_volume_limit"
	reasonPublishLimit        = "publish_limit"
	reasonTooLarge            = "too_large"
	reasonInvalidTar          = "invalid_tar"
	reasonDigestMismatch      = "digest_mismatch"
	reasonPublishInterrupted  = "publish_interrupted"
)

// publishRetryAfter is the Retry-After of a publish that may work later: a
// publish takes seconds to minutes.
const publishRetryAfter = "30"

// Bounds on reading a publish's body, instead of the server's ReadTimeout.
// Vars so tests can shrink them.
var (
	uploadStallTimeout = time.Minute
	uploadTimeout      = time.Hour
)

// RouterOption configures NewGatewayRouter.
type RouterOption func(*routerOptions)

type routerOptions struct {
	sharedVolumes *volumes.Service
}

// WithSharedVolumes serves the shared volume routes from svc. Without it, or
// with nil, they answer 501: no volume store is configured.
func WithSharedVolumes(svc *volumes.Service) RouterOption {
	return func(options *routerOptions) { options.sharedVolumes = svc }
}

func registerSharedVolumeRoutes(mux *http.ServeMux, svc *volumes.Service) {
	mux.HandleFunc("GET "+sharedVolumesPath, handleListSharedVolumes(svc))
	mux.HandleFunc("PUT "+sharedVolumesPath+"/{ref}", handlePutSharedVolume(svc))
	mux.HandleFunc("GET "+sharedVolumesPath+"/{ref}", handleGetSharedVolume(svc))
	mux.HandleFunc("DELETE "+sharedVolumesPath+"/{ref}", handleDeleteSharedVolume(svc))
}

// mayReadSharedVolumes and mayWriteSharedVolumes are the roles the shared
// volume routes take. Tenant keys get neither for now.
func mayReadSharedVolumes(role authRole) bool {
	return role == roleAdmin || role == roleVolumePublisher
}
func mayWriteSharedVolumes(role authRole) bool {
	return role == roleAdmin || role == roleVolumePublisher
}

// volumeRoles are the roles that may change shared volumes, as the volumes
// service names them.
var volumeRoles = map[authRole]volumes.Role{
	roleAdmin:           volumes.RoleAdmin,
	roleVolumePublisher: volumes.RoleVolumePublisher,
}

// sharedVolumeAccess answers a caller whose role allowed refuses, or any
// caller when there is no volume store, and returns the caller's role for
// the volumes service. ok is false when it answered.
func sharedVolumeAccess(w http.ResponseWriter, r *http.Request, svc *volumes.Service, allowed func(authRole) bool) (role volumes.Role, ok bool) {
	id, authenticated := authFromContext(r.Context())
	if !authenticated || !allowed(id.Role) {
		writeError(w, http.StatusForbidden, "shared volumes require an admin or volume publisher API key")
		return "", false
	}
	if svc == nil {
		writeErrorReason(w, http.StatusNotImplemented, "shared volumes need a volume store, and none is configured (SANDBOX_API_VOLUME_STORE)", reasonVolumeStoreDisabled)
		return "", false
	}
	return volumeRoles[id.Role], true
}

// sharedVolumeDigest reads the {ref} path segment: sha256:<64 lowercase hex>.
func sharedVolumeDigest(w http.ResponseWriter, r *http.Request) (string, bool) {
	digest, ok := strings.CutPrefix(r.PathValue("ref"), "sha256:")
	if !ok || !volume.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "invalid shared volume: expected sha256:<64 lowercase hex digits>")
		return "", false
	}
	return digest, true
}

// sharedVolumeName reads the optional name query parameter, the only one the
// shared volume routes take.
func sharedVolumeName(w http.ResponseWriter, r *http.Request) (string, bool) {
	query := r.URL.Query()
	for param := range query {
		if param != "name" {
			writeError(w, http.StatusBadRequest, "the only query parameter is name")
			return "", false
		}
	}
	names := query["name"]
	switch {
	case len(names) == 0:
		return "", true
	case len(names) > 1:
		writeError(w, http.StatusBadRequest, "name may be given once")
		return "", false
	case !volumes.ValidName(names[0]):
		writeError(w, http.StatusBadRequest, volumes.ErrInvalidName.Error())
		return "", false
	}
	return names[0], true
}

type sharedVolumeResponse struct {
	Digest      string             `json:"digest"`
	Name        string             `json:"name,omitempty"`
	Bytes       int64              `json:"bytes"`
	Tar         volume.TarCounts   `json:"tar"`
	Image       *sharedVolumeImage `json:"image,omitempty"`
	PublishedAt int64              `json:"published_at"`
}

type sharedVolumeImage struct {
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest"`
}

func sharedVolumeResponseOf(row *store.SharedVolume) sharedVolumeResponse {
	response := sharedVolumeResponse{
		Digest: "sha256:" + row.SHA256,
		Name:   row.Name,
		Bytes:  row.TarBytes,
		Tar: volume.TarCounts{
			Entries:           row.TarEntries,
			ContentBytes:      row.TarContentBytes,
			DecompressedBytes: row.TarDecompressedBytes,
		},
		PublishedAt: row.ReadyAt,
	}
	if row.ImageBytes > 0 {
		response.Image = &sharedVolumeImage{Bytes: row.ImageBytes, Digest: "sha256:" + row.ImageSHA256}
	}
	return response
}

func handlePutSharedVolume(svc *volumes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := sharedVolumeAccess(w, r, svc, mayWriteSharedVolumes)
		if !ok {
			return
		}
		digest, ok := sharedVolumeDigest(w, r)
		if !ok {
			return
		}
		name, ok := sharedVolumeName(w, r)
		if !ok {
			return
		}
		if r.ContentLength < 0 {
			writeError(w, http.StatusLengthRequired, "Content-Length is required")
			return
		}
		obs.FieldsFrom(r.Context()).Add("sha256", digest, "shared_volume_name", name)

		body := newUploadReader(w, r.Body)
		published, err := svc.Publish(r.Context(), volumes.PublishRequest{
			SHA256:     digest,
			Name:       name,
			Size:       r.ContentLength,
			Body:       body,
			Role:       role,
			UploadDone: body.done,
			Interrupt:  body.interrupt,
		})
		if err != nil {
			writeSharedVolumeError(w, r, err)
			return
		}
		status := http.StatusOK
		if published.Created {
			status = http.StatusCreated
			w.Header().Set("Location", sharedVolumesPath+"/sha256:"+digest)
		}
		writeJSON(w, status, sharedVolumeResponseOf(published.Volume))
	}
}

func handleGetSharedVolume(svc *volumes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := sharedVolumeAccess(w, r, svc, mayReadSharedVolumes); !ok {
			return
		}
		digest, ok := sharedVolumeDigest(w, r)
		if !ok {
			return
		}
		row, err := svc.Get(r.Context(), digest)
		if err != nil {
			writeSharedVolumeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, sharedVolumeResponseOf(row))
	}
}

func handleListSharedVolumes(svc *volumes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := sharedVolumeAccess(w, r, svc, mayReadSharedVolumes); !ok {
			return
		}
		name, ok := sharedVolumeName(w, r)
		if !ok {
			return
		}
		rows, err := svc.List(r.Context(), name)
		if err != nil {
			writeSharedVolumeError(w, r, err)
			return
		}
		responses := make([]sharedVolumeResponse, len(rows))
		for index, row := range rows {
			responses[index] = sharedVolumeResponseOf(row)
		}
		writeJSON(w, http.StatusOK, responses)
	}
}

func handleDeleteSharedVolume(svc *volumes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := sharedVolumeAccess(w, r, svc, mayWriteSharedVolumes)
		if !ok {
			return
		}
		digest, ok := sharedVolumeDigest(w, r)
		if !ok {
			return
		}
		obs.FieldsFrom(r.Context()).Add("sha256", digest)
		if err := svc.Delete(r.Context(), digest, role); err != nil {
			writeSharedVolumeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeSharedVolumeError answers err. Its text goes to the caller only for
// errors whose messages are fixed.
func writeSharedVolumeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, volumes.ErrNotFound):
		writeError(w, http.StatusNotFound, "shared volume not found")
	case errors.Is(err, volumes.ErrInvalidName):
		writeError(w, http.StatusBadRequest, volumes.ErrInvalidName.Error())
	case errors.Is(err, volumes.ErrPublishInProgress):
		w.Header().Set("Retry-After", publishRetryAfter)
		writeErrorReason(w, http.StatusConflict, volumes.ErrPublishInProgress.Error(), reasonPublishInProgress)
	case errors.Is(err, volumes.ErrNameTaken):
		writeErrorReason(w, http.StatusConflict, volumes.ErrNameTaken.Error(), reasonNameTaken)
	case errors.Is(err, volumes.ErrNameMismatch):
		writeErrorReason(w, http.StatusConflict, volumes.ErrNameMismatch.Error(), reasonNameMismatch)
	case errors.Is(err, volumes.ErrVolumeLimit):
		writeErrorReason(w, http.StatusConflict, volumes.ErrVolumeLimit.Error(), reasonSharedVolumeLimit)
	case errors.Is(err, volumes.ErrPublishLimit):
		w.Header().Set("Retry-After", publishRetryAfter)
		writeErrorReason(w, http.StatusTooManyRequests, volumes.ErrPublishLimit.Error(), reasonPublishLimit)
	case errors.Is(err, volumes.ErrTooLarge):
		writeErrorReason(w, http.StatusRequestEntityTooLarge, err.Error(), reasonTooLarge)
	case errors.Is(err, volumes.ErrInvalidTar):
		writeErrorReason(w, http.StatusUnprocessableEntity, err.Error(), reasonInvalidTar)
	case errors.Is(err, volumes.ErrDigestMismatch):
		writeErrorReason(w, http.StatusUnprocessableEntity, volumes.ErrDigestMismatch.Error(), reasonDigestMismatch)
	case errors.Is(err, volumes.ErrUpload) && errors.Is(err, os.ErrDeadlineExceeded):
		writeError(w, http.StatusRequestTimeout, "the upload stalled for a minute or took longer than an hour")
	case errors.Is(err, volumes.ErrUpload):
		writeError(w, http.StatusBadRequest, "failed to read request body")
	case errors.Is(err, volumes.ErrInterrupted):
		writeErrorReason(w, http.StatusServiceUnavailable, volumes.ErrInterrupted.Error(), reasonPublishInterrupted)
	default:
		slog.ErrorContext(r.Context(), "shared volume request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "shared volume store failure")
	}
}

// uploadReader reads a publish's body. It moves the connection's read
// deadline ahead before each read, so the upload ends when it stalls or runs
// past uploadTimeout rather than at the server's ReadTimeout.
type uploadReader struct {
	body       io.Reader
	controller *http.ResponseController
	deadline   time.Time
	// mutex keeps extend from undoing interrupt, and interrupt from acting
	// once done.
	mutex       sync.Mutex
	interrupted bool
	finished    bool
}

func newUploadReader(w http.ResponseWriter, body io.Reader) *uploadReader {
	upload := &uploadReader{
		body:       body,
		controller: http.NewResponseController(w),
		deadline:   time.Now().Add(uploadTimeout),
	}
	upload.extend()
	return upload
}

func (upload *uploadReader) extend() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	if upload.interrupted {
		return
	}
	// ErrNotSupported only outside a real server, as in tests.
	next := time.Now().Add(uploadStallTimeout)
	if next.After(upload.deadline) {
		next = upload.deadline
	}
	_ = upload.controller.SetReadDeadline(next)
}

func (upload *uploadReader) Read(buf []byte) (int, error) {
	upload.extend()
	return upload.body.Read(buf)
}

// interrupt ends a read waiting for the client now, and fails every later one.
func (upload *uploadReader) interrupt() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	if upload.finished {
		return
	}
	upload.interrupted = true
	_ = upload.controller.SetReadDeadline(time.Now())
}

// done clears the read deadline once the body is read, so the server's own
// read during the rest of the publish notices only a client that went away.
func (upload *uploadReader) done() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	upload.finished = true
	_ = upload.controller.SetReadDeadline(time.Time{})
}
