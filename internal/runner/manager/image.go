package manager

import "errors"

// ErrImageNotFound is returned when a custom image ID or tag does not exist.
var ErrImageNotFound = errors.New("image not found")

// ErrImageInUse is returned when an image is referenced by running sandboxes.
var ErrImageInUse = errors.New("image is in use by running sandboxes")

// BuildImageOptions controls custom image creation.
type BuildImageOptions struct {
	BaseImage       string
	DockerfileSteps []string
}
