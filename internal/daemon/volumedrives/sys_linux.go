//go:build linux

package volumedrives

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// System is the real system calls.
func System() Sys {
	return linuxSys{}
}

type linuxSys struct{}

func (linuxSys) DeviceSize(device string) (uint64, error) {
	f, err := os.Open(device)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	return uint64(size), err
}

func (linuxSys) FlushBuffers(device string) error {
	f, err := os.Open(device)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.IoctlSetInt(int(f.Fd()), unix.BLKFLSBUF, 0)
}

func (linuxSys) MakeMountPoint(path string) error {
	return mkdirNoFollow(path)
}

// Mount goes by path: mkdirNoFollow found no symlink on it, and nothing else
// runs in the guest while the daemon holds root.
func (linuxSys) Mount(device, path string) error {
	return unix.Mount(device, path, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, "noload")
}

func (linuxSys) Unmount(path string) error {
	return unix.Unmount(path, unix.MNT_DETACH)
}
