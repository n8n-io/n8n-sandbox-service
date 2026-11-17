//go:build !linux

package volumebuild

import (
	"errors"
	"io"
	"os"
)

// System fails every call but the gate's: builder microVMs run Linux. It lets
// the package build and its tests run on macOS.
func System() Sys {
	return unsupportedSys{}
}

type unsupportedSys struct{}

func (unsupportedSys) Pid() int  { return os.Getpid() }
func (unsupportedSys) Euid() int { return os.Geteuid() }

func (unsupportedSys) Tar(string, int64) (io.ReadCloser, error) { return nil, errors.ErrUnsupported }
func (unsupportedSys) Format(string, int64) error               { return errors.ErrUnsupported }
func (unsupportedSys) Mount(string, string) error               { return errors.ErrUnsupported }
func (unsupportedSys) UsedBytes(string) (int64, error)          { return 0, errors.ErrUnsupported }
func (unsupportedSys) Flush(string) error                       { return errors.ErrUnsupported }
func (unsupportedSys) Unmount(string) error                     { return errors.ErrUnsupported }
func (unsupportedSys) Check(string) error                       { return errors.ErrUnsupported }
func (unsupportedSys) WriteStatus(string, []byte) error         { return errors.ErrUnsupported }
func (unsupportedSys) Reboot() error                            { return errors.ErrUnsupported }
