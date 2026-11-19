package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

// sectorBytes is the unit a Firecracker drive's size comes in.
const sectorBytes = 512

// Volume is a read-only volume a sandbox gets at create. Its position in
// CreateOptions.Volumes is the drive it gets on Firecracker.
type Volume struct {
	Scope  string      `json:"scope"`
	SHA256 string      `json:"sha256"`
	Size   int64       `json:"size"`
	Kind   volume.Kind `json:"kind"`
	// URL reads the object. Its query carries a token, so it never reaches a
	// log, an error, a metric or the echo.
	URL       string `json:"url"`
	MountPath string `json:"mount_path"`
	// Optional lets the sandbox start without the volume when the volume
	// itself fails; a required volume that fails fails the create.
	Optional bool `json:"optional,omitempty"`
	// Tar is what the API counted in a tar volume; only tar volumes have it.
	// The runner unpacks the tar within these counts.
	Tar volume.TarCounts `json:"tar,omitzero"`
}

// String leaves out the URL.
func (v Volume) String() string {
	return fmt.Sprintf("{scope:%s sha256:%s size:%d kind:%s mount_path:%s optional:%t}",
		v.Scope, v.SHA256, v.Size, v.Kind, v.MountPath, v.Optional)
}

// GoString leaves out the URL.
func (v Volume) GoString() string {
	return v.String()
}

// LogValue leaves out the URL, which a JSON log handler would otherwise
// marshal.
func (v Volume) LogValue() slog.Value {
	return slog.StringValue(v.String())
}

// Volumes is a list of volumes that logs without their URLs.
type Volumes []Volume

// LogValue leaves out the URLs.
func (volumes Volumes) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%v", []Volume(volumes)))
}

// LogValue leaves out the volumes' URLs.
func (o CreateOptions) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%+v", struct {
		Egress  Egress
		Volumes []Volume
	}{o.Egress, o.Volumes}))
}

func (v Volume) validate() error {
	if !volume.ValidScope(v.Scope) {
		return fmt.Errorf("invalid scope %q", v.Scope)
	}
	if !volume.ValidDigest(v.SHA256) {
		return errors.New("sha256 must be 64 lowercase hex characters")
	}
	if v.Size <= 0 {
		return fmt.Errorf("size must be positive, got %d", v.Size)
	}
	if !v.Kind.Valid() {
		return fmt.Errorf("unknown kind %q", v.Kind)
	}
	if v.Kind == volume.KindExt4 && v.Size%sectorBytes != 0 {
		return fmt.Errorf("an ext4 image is whole %d-byte sectors, got %d bytes", sectorBytes, v.Size)
	}
	if v.Kind == volume.KindTar {
		if err := v.Tar.Validate(); err != nil {
			return fmt.Errorf("tar counts: %w", err)
		}
	} else if v.Tar != (volume.TarCounts{}) {
		return errors.New("only a tar volume has tar counts")
	}
	// Never quoted: the URL carries a token, and url.Parse's error quotes it.
	parsed, err := url.Parse(v.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("url must be an https URL with a host")
	}
	if err := volume.ValidateMountPath(v.MountPath); err != nil {
		return fmt.Errorf("mount path: %w", err)
	}
	return nil
}

func validateVolumes(volumes []Volume) error {
	if len(volumes) > volume.MaxDrives {
		return fmt.Errorf("at most %d volumes, got %d", volume.MaxDrives, len(volumes))
	}
	for index, vol := range volumes {
		if err := vol.validate(); err != nil {
			return fmt.Errorf("volume %d: %w", index, err)
		}
		for prevIndex, prev := range volumes[:index] {
			if prev.Scope == vol.Scope && prev.SHA256 == vol.SHA256 {
				return fmt.Errorf("volume %d: same object as volume %d", index, prevIndex)
			}
			if volume.MountPathsOverlap(prev.MountPath, vol.MountPath) {
				return fmt.Errorf("volume %d: mount path %q overlaps %q", index, vol.MountPath, prev.MountPath)
			}
		}
	}
	return nil
}

// Why an optional volume was left out. Only these reach the echo and metric
// labels.
const (
	// VolumeLeftOutCacheFull: the runner's volume cache has no room for it.
	VolumeLeftOutCacheFull = "cache_full"
	// VolumeLeftOutTimeout: its download did not finish in time. It goes on
	// in the cache, for a retry.
	VolumeLeftOutTimeout = "timeout"
	// VolumeLeftOutDownload: it could not be downloaded, or did not match its
	// size or sha256.
	VolumeLeftOutDownload = "download"
	// VolumeLeftOutUnpack: its tar breaks the volume tar rules or holds more
	// than its counts.
	VolumeLeftOutUnpack = "unpack"
	// VolumeLeftOutDrive: the guest did not see its drive at its size.
	VolumeLeftOutDrive = "drive"
	// VolumeLeftOutMountPoint: the guest could not create its mount point.
	VolumeLeftOutMountPoint = "mount_point"
	// VolumeLeftOutMount: the guest could not mount it.
	VolumeLeftOutMount = "mount"
)

// VolumeLeftOutReasons lists the VolumeLeftOut values.
var VolumeLeftOutReasons = []string{
	VolumeLeftOutCacheFull, VolumeLeftOutTimeout, VolumeLeftOutDownload, VolumeLeftOutUnpack,
	VolumeLeftOutDrive, VolumeLeftOutMountPoint, VolumeLeftOutMount,
}

// AppliedCreateOptions is the JSON shape of applied_create_options_json: the
// create options as the runtime applied them.
type AppliedCreateOptions struct {
	Egress      Egress           `json:"egress,omitempty"`
	Volumes     []AppliedVolume  `json:"volumes,omitempty"`
	Template    string           `json:"template,omitempty"`
	EgressProxy *EgressProxySpec `json:"egress_proxy,omitempty"`
	Tenant      string           `json:"tenant,omitempty"`
}

// AppliedVolume is a requested volume, without its URL, and whether the
// sandbox has it.
type AppliedVolume struct {
	Scope     string      `json:"scope"`
	SHA256    string      `json:"sha256"`
	Size      int64       `json:"size"`
	Kind      volume.Kind `json:"kind"`
	MountPath string      `json:"mount_path"`
	Optional  bool        `json:"optional,omitempty"`
	Attached  bool        `json:"attached"`
	// Reason is why the volume was left out, one of the VolumeLeftOut values.
	Reason string `json:"reason,omitempty"`
}

// Applied is the echo of a create that left out the volumes in leftOut, which
// maps an index in o.Volumes to a VolumeLeftOut value.
func (o *CreateOptions) Applied(leftOut map[int]string) AppliedCreateOptions {
	applied := AppliedCreateOptions{
		Egress:      o.Egress,
		Template:    o.Template,
		EgressProxy: o.EgressProxy,
		Tenant:      o.Tenant,
	}
	for index, vol := range o.Volumes {
		reason, isLeftOut := leftOut[index]
		applied.Volumes = append(applied.Volumes, AppliedVolume{
			Scope:     vol.Scope,
			SHA256:    vol.SHA256,
			Size:      vol.Size,
			Kind:      vol.Kind,
			MountPath: vol.MountPath,
			Optional:  vol.Optional,
			Attached:  !isLeftOut,
			Reason:    reason,
		})
	}
	return applied
}
