// This file serves the shared volume routes: publish, get, list and delete.

package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

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

// publishRetryAfterSeconds is the Retry-After of a publish that may work
// later: a publish takes seconds to minutes.
const publishRetryAfterSeconds = "30"

// uploadStallTimeout frees a publish slot of the pod soon after a client stops
// sending: a live client sends within seconds. A var so tests can shrink it.
var uploadStallTimeout = time.Minute

// uploadTimeout lets a tar of MaxUploadBytes (4 GiB) arrive at about 1.2 MB/s.
// Both bounds replace the server's ReadTimeout for a publish.
const uploadTimeout = time.Hour

// RouterOption configures NewGatewayRouter.
type RouterOption func(*routerOptions)

type routerOptions struct {
	sharedVolumes *volumes.Service
}

// WithSharedVolumes serves the shared volume routes from svc. Without it, or
// with nil, they answer 501: no volume store is configured.
func WithSharedVolumes(service *volumes.Service) RouterOption {
	return func(options *routerOptions) { options.sharedVolumes = service }
}

func registerSharedVolumeRoutes(mux *http.ServeMux, service *volumes.Service) {
	mux.HandleFunc("GET "+sharedVolumesPath, handleListSharedVolumes(service))
	mux.HandleFunc("PUT "+sharedVolumesPath+"/{ref}", handlePutSharedVolume(service))
	mux.HandleFunc("GET "+sharedVolumesPath+"/{ref}", handleGetSharedVolume(service))
	mux.HandleFunc("DELETE "+sharedVolumesPath+"/{ref}", handleDeleteSharedVolume(service))
}

// mayUseSharedVolumes reports whether a role may use the shared volume routes.
// Tenant, provisioner and scaler keys get 403 on every one of them.
func mayUseSharedVolumes(role authRole) bool {
	return role == roleAdmin || role == roleVolumePublisher
}

// volumeRolesByAuthRole maps the roles that may use shared volumes to the
// names the volumes service uses.
var volumeRolesByAuthRole = map[authRole]volumes.Role{
	roleAdmin:           volumes.RoleAdmin,
	roleVolumePublisher: volumes.RoleVolumePublisher,
}

// requireSharedVolumeAccess returns the caller's role for the volumes service.
// It answers 403 when the role may not use shared volumes, and 501 when there
// is no volume store. Then ok is false.
func requireSharedVolumeAccess(writer http.ResponseWriter, request *http.Request, service *volumes.Service) (role volumes.Role, ok bool) {
	id, authenticated := authFromContext(request.Context())
	if !authenticated || !mayUseSharedVolumes(id.Role) {
		writeError(writer, http.StatusForbidden, "shared volumes require an admin or volume publisher API key")
		return "", false
	}
	if service == nil {
		writeErrorReason(writer, http.StatusNotImplemented, "shared volumes need a volume store, and none is configured (SANDBOX_API_VOLUME_STORE)", reasonVolumeStoreDisabled)
		return "", false
	}
	return volumeRolesByAuthRole[id.Role], true
}

// parseSharedVolumeDigest reads the {ref} path segment: sha256:<64 lowercase hex>.
func parseSharedVolumeDigest(writer http.ResponseWriter, request *http.Request) (string, bool) {
	digest, ok := strings.CutPrefix(request.PathValue("ref"), "sha256:")
	if !ok || !volume.ValidDigest(digest) {
		writeError(writer, http.StatusBadRequest, "invalid shared volume: expected sha256:<64 lowercase hex digits>")
		return "", false
	}
	return digest, true
}

// parseSharedVolumeNameQuery reads the optional name query parameter, the only one the
// shared volume routes take.
func parseSharedVolumeNameQuery(writer http.ResponseWriter, request *http.Request) (string, bool) {
	// URL.Query would drop a malformed parameter and so take the request for
	// one without a name.
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "malformed query string")
		return "", false
	}
	for param := range query {
		if param != "name" {
			writeError(writer, http.StatusBadRequest, "the only query parameter is name")
			return "", false
		}
	}
	names := query["name"]
	switch {
	case len(names) == 0:
		return "", true
	case len(names) > 1:
		writeError(writer, http.StatusBadRequest, "name may be given once")
		return "", false
	case !volumes.ValidName(names[0]):
		writeError(writer, http.StatusBadRequest, volumes.ErrInvalidName.Error())
		return "", false
	}
	return names[0], true
}

type sharedVolumeResponse struct {
	Digest      string           `json:"digest"`
	Name        string           `json:"name,omitempty"`
	Bytes       int64            `json:"bytes"`
	Tar         volume.TarCounts `json:"tar"`
	PublishedAt int64            `json:"published_at"`
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
	return response
}

func handlePutSharedVolume(service *volumes.Service) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		role, ok := requireSharedVolumeAccess(writer, request, service)
		if !ok {
			return
		}
		digest, ok := parseSharedVolumeDigest(writer, request)
		if !ok {
			return
		}
		name, ok := parseSharedVolumeNameQuery(writer, request)
		if !ok {
			return
		}
		if request.ContentLength < 0 {
			writeError(writer, http.StatusLengthRequired, "Content-Length is required")
			return
		}
		obs.FieldsFrom(request.Context()).Add("sha256", digest, "shared_volume_name", name)

		body := newUploadReader(writer, request.Body)
		published, err := service.Publish(request.Context(), volumes.PublishRequest{
			SHA256:     digest,
			Name:       name,
			SizeBytes:  request.ContentLength,
			Body:       body,
			Role:       role,
			UploadDone: body.finishUpload,
			Interrupt:  body.interrupt,
		})
		if err != nil {
			writeSharedVolumeError(writer, request, err)
			return
		}
		status := http.StatusOK
		if published.Created {
			status = http.StatusCreated
			writer.Header().Set("Location", sharedVolumesPath+"/sha256:"+digest)
		}
		writeJSON(writer, status, sharedVolumeResponseOf(published.Volume))
	}
}

func handleGetSharedVolume(service *volumes.Service) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := requireSharedVolumeAccess(writer, request, service); !ok {
			return
		}
		digest, ok := parseSharedVolumeDigest(writer, request)
		if !ok {
			return
		}
		row, err := service.Get(request.Context(), digest)
		if err != nil {
			writeSharedVolumeError(writer, request, err)
			return
		}
		writeJSON(writer, http.StatusOK, sharedVolumeResponseOf(row))
	}
}

func handleListSharedVolumes(service *volumes.Service) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := requireSharedVolumeAccess(writer, request, service); !ok {
			return
		}
		name, ok := parseSharedVolumeNameQuery(writer, request)
		if !ok {
			return
		}
		rows, err := service.List(request.Context(), name)
		if err != nil {
			writeSharedVolumeError(writer, request, err)
			return
		}
		responses := make([]sharedVolumeResponse, len(rows))
		for index, row := range rows {
			responses[index] = sharedVolumeResponseOf(row)
		}
		writeJSON(writer, http.StatusOK, responses)
	}
}

func handleDeleteSharedVolume(service *volumes.Service) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		role, ok := requireSharedVolumeAccess(writer, request, service)
		if !ok {
			return
		}
		digest, ok := parseSharedVolumeDigest(writer, request)
		if !ok {
			return
		}
		obs.FieldsFrom(request.Context()).Add("sha256", digest)
		if err := service.Delete(request.Context(), digest, role); err != nil {
			writeSharedVolumeError(writer, request, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}
}

// writeSharedVolumeError answers err. Its text goes to the caller only for
// errors whose messages are fixed.
func writeSharedVolumeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, volumes.ErrNotFound):
		writeError(writer, http.StatusNotFound, "shared volume not found")
	case errors.Is(err, volumes.ErrInvalidName):
		writeError(writer, http.StatusBadRequest, volumes.ErrInvalidName.Error())
	case errors.Is(err, volumes.ErrPublishInProgress):
		writer.Header().Set("Retry-After", publishRetryAfterSeconds)
		writeErrorReason(writer, http.StatusConflict, volumes.ErrPublishInProgress.Error(), reasonPublishInProgress)
	case errors.Is(err, volumes.ErrNameTaken):
		writeErrorReason(writer, http.StatusConflict, volumes.ErrNameTaken.Error(), reasonNameTaken)
	case errors.Is(err, volumes.ErrNameMismatch):
		writeErrorReason(writer, http.StatusConflict, volumes.ErrNameMismatch.Error(), reasonNameMismatch)
	case errors.Is(err, volumes.ErrVolumeLimit):
		writeErrorReason(writer, http.StatusConflict, volumes.ErrVolumeLimit.Error(), reasonSharedVolumeLimit)
	case errors.Is(err, volumes.ErrPublishLimit):
		writer.Header().Set("Retry-After", publishRetryAfterSeconds)
		writeErrorReason(writer, http.StatusTooManyRequests, volumes.ErrPublishLimit.Error(), reasonPublishLimit)
	case errors.Is(err, volumes.ErrTooLarge):
		writeErrorReason(writer, http.StatusRequestEntityTooLarge, err.Error(), reasonTooLarge)
	case errors.Is(err, volumes.ErrInvalidTar):
		writeErrorReason(writer, http.StatusUnprocessableEntity, err.Error(), reasonInvalidTar)
	case errors.Is(err, volumes.ErrDigestMismatch):
		writeErrorReason(writer, http.StatusUnprocessableEntity, volumes.ErrDigestMismatch.Error(), reasonDigestMismatch)
	case errors.Is(err, volumes.ErrUpload) && errors.Is(err, os.ErrDeadlineExceeded):
		writeError(writer, http.StatusRequestTimeout, "the upload stalled for a minute or took longer than an hour")
	case errors.Is(err, volumes.ErrUpload):
		writeError(writer, http.StatusBadRequest, "failed to read request body")
	case errors.Is(err, volumes.ErrInterrupted):
		writeErrorReason(writer, http.StatusServiceUnavailable, volumes.ErrInterrupted.Error(), reasonPublishInterrupted)
	default:
		slog.ErrorContext(request.Context(), "shared volume request failed", "error", err)
		writeError(writer, http.StatusInternalServerError, "shared volume store failure")
	}
}

// uploadReader reads a publish's body. It moves the connection's read
// deadline ahead before each read, so the upload ends when it stalls or runs
// past uploadTimeout rather than at the server's ReadTimeout. It ignores a
// failed SetReadDeadline, which only happens outside a real server, as in
// tests.
type uploadReader struct {
	body           io.Reader
	controller     *http.ResponseController
	uploadDeadline time.Time
	// mutex keeps extendReadDeadline from undoing interrupt, and interrupt
	// from acting after finishUpload.
	mutex          sync.Mutex
	interrupted    bool
	uploadFinished bool
}

func newUploadReader(writer http.ResponseWriter, body io.Reader) *uploadReader {
	upload := &uploadReader{
		body:           body,
		controller:     http.NewResponseController(writer),
		uploadDeadline: time.Now().Add(uploadTimeout),
	}
	upload.extendReadDeadline()
	return upload
}

func (upload *uploadReader) extendReadDeadline() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	if upload.interrupted {
		return
	}
	next := time.Now().Add(uploadStallTimeout)
	if next.After(upload.uploadDeadline) {
		next = upload.uploadDeadline
	}
	_ = upload.controller.SetReadDeadline(next)
}

func (upload *uploadReader) Read(buffer []byte) (int, error) {
	upload.extendReadDeadline()
	return upload.body.Read(buffer)
}

// interrupt ends a read waiting for the client now, and fails every later one.
func (upload *uploadReader) interrupt() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	if upload.uploadFinished {
		return
	}
	upload.interrupted = true
	_ = upload.controller.SetReadDeadline(time.Now())
}

// finishUpload clears the read deadline once the body is read, so the server's own
// read during the rest of the publish notices only a client that went away.
func (upload *uploadReader) finishUpload() {
	upload.mutex.Lock()
	defer upload.mutex.Unlock()
	upload.uploadFinished = true
	_ = upload.controller.SetReadDeadline(time.Time{})
}
