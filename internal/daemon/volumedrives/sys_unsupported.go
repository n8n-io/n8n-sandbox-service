//go:build !linux

package volumedrives

import "errors"

// System fails every call: volume drives exist only in Linux guests. It lets
// the package build and its tests run on macOS.
func System() Sys {
	return unsupportedSys{}
}

type unsupportedSys struct{}

func (unsupportedSys) DeviceSize(string) (uint64, error) { return 0, errors.ErrUnsupported }
func (unsupportedSys) FlushBuffers(string) error         { return errors.ErrUnsupported }
func (unsupportedSys) MakeMountPoint(string) error       { return errors.ErrUnsupported }
func (unsupportedSys) Mount(string, string) error        { return errors.ErrUnsupported }
func (unsupportedSys) Unmount(string) error              { return errors.ErrUnsupported }
