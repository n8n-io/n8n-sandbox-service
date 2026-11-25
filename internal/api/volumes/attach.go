// This file finds the shared volumes a sandbox create asks for, gets their
// read URLs for the runner and reads back what the runner attached.

package volumes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// MaxVolumesPerSandbox is the most volumes a create may ask for, those left
// out included: the volume drives of the golden snapshot.
const MaxVolumesPerSandbox = 8

// readURLTTL is how long a runner may start a download with its read URL. A
// runner starts its downloads within seconds of the create; the rest is margin
// for clock skew.
const readURLTTL = 10 * time.Minute

// readURLTimeout bounds the calls to the volume store for one create's read
// URLs. A call takes milliseconds.
const readURLTimeout = 10 * time.Second

// Why a volume was left out, besides the runner's VolumeLeftOut values.
const (
	LeftOutNotFound         = "not_found"
	LeftOutNoImage          = "no_image"
	LeftOutTooLarge         = "too_large"
	LeftOutStoreUnavailable = "store_unavailable"
	// LeftOutError stands for a reason from a runner the API doesn't know.
	LeftOutError = "error"
)

// Request is one volume a create asks for, by Name or by Digest
// ("sha256:" and 64 lowercase hex digits).
type Request struct {
	Name      string `json:"name,omitempty"`
	Digest    string `json:"digest,omitempty"`
	MountPath string `json:"mount_path"`
	Optional  bool   `json:"optional,omitempty"`
}

// SandboxVolume is a volume a create asked for and whether the sandbox has
// it. It is kept with the sandbox.
type SandboxVolume struct {
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Scope is empty for a volume that wasn't found.
	Scope     string `json:"scope,omitempty"`
	MountPath string `json:"mount_path"`
	Optional  bool   `json:"optional"`
	Attached  bool   `json:"attached"`
	// Reason is why a volume that isn't attached was left out.
	Reason string `json:"reason,omitempty"`
}

// RequestError is a create's volumes that break a rule. Its message names the
// entry and may go to the caller.
type RequestError struct{ message string }

func (e *RequestError) Error() string { return e.message }

func requestError(index int, format string, args ...any) error {
	return &RequestError{message: fmt.Sprintf("volumes[%d]: ", index) + fmt.Sprintf(format, args...)}
}

// RequiredError is a required volume the sandbox can't have, for Reason, a
// LeftOut value. Nothing was created.
type RequiredError struct {
	Index  int
	Reason string
}

func (e *RequiredError) Error() string {
	switch e.Reason {
	case LeftOutNotFound:
		return fmt.Sprintf("volumes[%d]: no volume with this name or digest", e.Index)
	case LeftOutNoImage:
		return fmt.Sprintf("volumes[%d]: the volume has no image for this service's runners; publish it again", e.Index)
	case LeftOutTooLarge:
		return fmt.Sprintf("volumes[%d]: with the required volumes before it, more than %d bytes on a runner", e.Index, volume.MaxSandboxVolumeBytes)
	default:
		return fmt.Sprintf("volumes[%d]: the volume store could not hand it to the runner; try again", e.Index)
	}
}

// ErrVolumesMismatch is a runner's echo that differs from the volumes sent.
var ErrVolumesMismatch = errors.New("the runner did not apply the requested volumes")

// ValidateRequests checks a create's volumes against every rule that needs no
// lookup.
func ValidateRequests(requests []Request) error {
	if len(requests) > MaxVolumesPerSandbox {
		return &RequestError{message: fmt.Sprintf("volumes: at most %d, got %d", MaxVolumesPerSandbox, len(requests))}
	}
	for index, request := range requests {
		switch {
		case (request.Name == "") == (request.Digest == ""):
			return requestError(index, "give exactly one of name and digest")
		case request.Name != "" && !ValidName(request.Name):
			return requestError(index, "invalid name: 1 to %d bytes of a-z, 0-9, '.', '_' and '-', starting and ending with a letter or digit", MaxNameBytes)
		case request.Digest != "" && !validDigestRef(request.Digest):
			return requestError(index, "invalid digest: expected sha256:<64 lowercase hex digits>")
		}
		if err := volume.ValidateMountPath(request.MountPath); err != nil {
			return requestError(index, "mount_path: %v", err)
		}
		for earlier, previous := range requests[:index] {
			if (request.Name != "" && request.Name == previous.Name) || (request.Digest != "" && request.Digest == previous.Digest) {
				return requestError(index, "the same volume as volumes[%d]", earlier)
			}
			if volume.MountPathsOverlap(previous.MountPath, request.MountPath) {
				return requestError(index, "mount_path %q overlaps volumes[%d]", request.MountPath, earlier)
			}
		}
	}
	return nil
}

func validDigestRef(ref string) bool {
	digest, ok := strings.CutPrefix(ref, "sha256:")
	return ok && volume.ValidDigest(digest)
}

// Matches reports whether the requests ask for the volumes a sandbox has, in
// any order: the same name or digest, mount path and optional.
func Matches(have []SandboxVolume, requests []Request) bool {
	if len(have) != len(requests) {
		return false
	}
	for _, request := range requests {
		if !slices.ContainsFunc(have, func(had SandboxVolume) bool {
			sameVolume := (request.Name != "" && request.Name == had.Name) || (request.Digest != "" && request.Digest == had.Digest)
			return sameVolume && request.MountPath == had.MountPath && request.Optional == had.Optional
		}) {
			return false
		}
	}
	return true
}

// Attacher finds the shared volumes of sandbox creates.
type Attacher struct {
	records store.SharedVolumes
	// objects is nil without a volume store: then no volume is found.
	objects volumestore.Store
	// kind is what the service's runners attach.
	kind volume.Kind
}

// NewAttacher finds volumes in records and gets their objects of kind from
// objects, which may be nil.
func NewAttacher(records store.SharedVolumes, objects volumestore.Store, kind volume.Kind) *Attacher {
	return &Attacher{records: records, objects: objects, kind: kind}
}

// Attachment is the volumes of one create.
type Attachment struct {
	// Volumes are the volumes the create asked for, in request order.
	Volumes []SandboxVolume
	runner  runnerruntime.Volumes
	// sent is the index in Volumes of each volume in runner.
	sent []int
}

// RunnerVolumes returns the volumes the runner is to attach.
func (att *Attachment) RunnerVolumes() runnerruntime.Volumes { return att.runner }

// Prepare finds the volumes of a create and gets a read URL for each one the
// runner is to attach. requests must be valid. A required volume that can't be
// attached returns *RequiredError, and two requests for one volume
// *RequestError. An optional volume is left out instead.
func (a *Attacher) Prepare(ctx context.Context, requests []Request) (*Attachment, error) {
	attachment := &Attachment{Volumes: make([]SandboxVolume, len(requests))}
	foundByIndex := make(map[int]*store.SharedVolume)
	indexByID := make(map[string]int)
	for index, request := range requests {
		attachment.Volumes[index] = SandboxVolume{Name: request.Name, Digest: request.Digest, MountPath: request.MountPath, Optional: request.Optional}
		found, err := a.find(ctx, request)
		if err != nil {
			return nil, err
		}
		reason := ""
		switch {
		case found == nil:
			reason = LeftOutNotFound
		case a.kind == volume.KindExt4 && found.ImageBytes == 0:
			reason = LeftOutNoImage
		}
		if found != nil {
			if earlier, seen := indexByID[found.ID]; seen {
				return nil, requestError(index, "the same volume as volumes[%d]", earlier)
			}
			indexByID[found.ID] = index
			attachment.Volumes[index].Name, attachment.Volumes[index].Digest = found.Name, "sha256:"+found.SHA256
			attachment.Volumes[index].Scope = volume.SharedScope
		}
		if reason != "" {
			if err := leaveOut(attachment, index, reason); err != nil {
				return nil, err
			}
			continue
		}
		foundByIndex[index] = found
	}

	// Required volumes first, then optional ones in request order while they
	// fit.
	var total int64
	for _, optional := range []bool{false, true} {
		for index, request := range requests {
			found, ok := foundByIndex[index]
			if !ok || request.Optional != optional {
				continue
			}
			if size := a.runnerBytes(found); total+size <= volume.MaxSandboxVolumeBytes {
				total += size
				continue
			}
			if err := leaveOut(attachment, index, LeftOutTooLarge); err != nil {
				return nil, err
			}
			delete(foundByIndex, index)
		}
	}

	urlCtx, cancel := context.WithTimeout(ctx, readURLTimeout)
	defer cancel()
	for index := range requests {
		found, ok := foundByIndex[index]
		if !ok {
			continue
		}
		url, err := a.objects.ReadURL(urlCtx, objectKey(found.ID, a.kind), readURLTTL)
		if err != nil {
			slog.WarnContext(ctx, "create sandbox: volume read URL failed", "index", index, "version_id", found.ID, "kind", a.kind, "error", err)
			if err := leaveOut(attachment, index, LeftOutStoreUnavailable); err != nil {
				return nil, err
			}
			continue
		}
		runnerVolume := runnerruntime.Volume{
			Scope: volume.SharedScope, SHA256: found.SHA256, Size: found.TarBytes, Kind: volume.KindTar, URL: url,
			MountPath: requests[index].MountPath, Optional: requests[index].Optional,
			Tar: volume.TarCounts{Entries: found.TarEntries, ContentBytes: found.TarContentBytes, DecompressedBytes: found.TarDecompressedBytes},
		}
		if a.kind == volume.KindExt4 {
			runnerVolume.SHA256, runnerVolume.Size, runnerVolume.Kind, runnerVolume.Tar = found.ImageSHA256, found.ImageBytes, volume.KindExt4, volume.TarCounts{}
		}
		attachment.runner = append(attachment.runner, runnerVolume)
		attachment.sent = append(attachment.sent, index)
	}
	return attachment, nil
}

// find returns the ready shared volume a request names, or nil.
func (a *Attacher) find(ctx context.Context, request Request) (*store.SharedVolume, error) {
	if a.objects == nil {
		return nil, nil
	}
	if request.Name != "" {
		found, err := a.records.ListReadySharedVolumes(ctx, request.Name)
		if err != nil || len(found) == 0 {
			return nil, err
		}
		return found[0], nil
	}
	found, err := a.records.GetLiveSharedVolume(ctx, strings.TrimPrefix(request.Digest, "sha256:"))
	if err != nil || found == nil || found.State != store.SharedVolumeReady {
		return nil, err
	}
	return found, nil
}

// runnerBytes is what a volume takes in a runner's volume cache.
func (a *Attacher) runnerBytes(found *store.SharedVolume) int64 {
	if a.kind == volume.KindExt4 {
		return found.ImageBytes
	}
	counts := volume.TarCounts{Entries: found.TarEntries, ContentBytes: found.TarContentBytes, DecompressedBytes: found.TarDecompressedBytes}
	return volume.RunnerBytes(volume.KindTar, found.TarBytes, counts)
}

// leaveOut leaves out the optional volume index for reason, or returns
// *RequiredError for a required one.
func leaveOut(attachment *Attachment, index int, reason string) error {
	if !attachment.Volumes[index].Optional {
		return &RequiredError{Index: index, Reason: reason}
	}
	attachment.Volumes[index].Reason = reason
	return nil
}

// Apply records the runner's echo of the volumes sent. An echo that differs
// from them, a runner's that ignored them included, returns an error wrapping
// ErrVolumesMismatch and records nothing.
func (att *Attachment) Apply(echo []runnerruntime.AppliedVolume) error {
	if len(echo) != len(att.runner) {
		return fmt.Errorf("%w: %d volumes echoed, %d sent", ErrVolumesMismatch, len(echo), len(att.runner))
	}
	for index, sent := range att.runner {
		applied := echo[index]
		switch {
		case applied.Scope != sent.Scope, applied.SHA256 != sent.SHA256, applied.Size != sent.Size, applied.Kind != sent.Kind,
			applied.MountPath != sent.MountPath, applied.Optional != sent.Optional:
			return fmt.Errorf("%w: volume %d differs from the one sent", ErrVolumesMismatch, index)
		case !applied.Attached && !sent.Optional:
			return fmt.Errorf("%w: required volume %d not attached", ErrVolumesMismatch, index)
		}
	}
	for index, applied := range echo {
		sandboxVolume := &att.Volumes[att.sent[index]]
		sandboxVolume.Attached = applied.Attached
		if !applied.Attached {
			sandboxVolume.Reason = applied.Reason
			if !slices.Contains(runnerruntime.VolumeLeftOutReasons, applied.Reason) {
				sandboxVolume.Reason = LeftOutError
			}
		}
	}
	return nil
}
