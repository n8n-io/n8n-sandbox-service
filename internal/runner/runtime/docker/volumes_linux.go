package docker

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// keptMountFlags maps the statfs flags of a mount to the mount flags a
// remount must repeat to keep them. A user namespace locks those a more
// privileged one set, and a remount without them fails.
var keptMountFlags = map[int64]uintptr{
	unix.ST_RDONLY:     unix.MS_RDONLY,
	unix.ST_NOEXEC:     unix.MS_NOEXEC,
	unix.ST_NOATIME:    unix.MS_NOATIME,
	unix.ST_NODIRATIME: unix.MS_NODIRATIME,
	unix.ST_RELATIME:   unix.MS_RELATIME,
}

// setNoSuidNoDev makes the mount dir is on nosuid,nodev. If it already is,
// there is nothing to do, which is also what a runner restarted in the same
// container finds. Otherwise dir is bind-mounted onto itself and that mount is
// remounted with both flags and those it had.
func setNoSuidNoDev(dir string) error {
	if dir == "" {
		return errors.New("no volume cache dir")
	}
	if checkNoSuidNoDev(dir) == nil {
		return nil
	}
	if err := unix.Mount(dir, dir, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind the dir onto itself: %w", err)
	}
	var stat unix.Statfs_t
	err := unix.Statfs(dir, &stat)
	if err == nil {
		flags := uintptr(unix.MS_REMOUNT | unix.MS_BIND | unix.MS_NOSUID | unix.MS_NODEV)
		for statFlag, mountFlag := range keptMountFlags {
			if int64(stat.Flags)&statFlag != 0 {
				flags |= mountFlag
			}
		}
		// A remount without an atime flag would turn strictatime into
		// relatime.
		if int64(stat.Flags)&(unix.ST_NOATIME|unix.ST_RELATIME) == 0 {
			flags |= unix.MS_STRICTATIME
		}
		err = unix.Mount("", dir, "", flags, "")
	}
	if err == nil {
		err = checkNoSuidNoDev(dir)
	}
	if err != nil {
		_ = unix.Unmount(dir, unix.MNT_DETACH)
		return fmt.Errorf("remount the dir nosuid,nodev: %w", err)
	}
	return nil
}

// checkNoSuidNoDev fails unless the mount path is on is nosuid and nodev.
func checkNoSuidNoDev(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("statfs: %w", err)
	}
	if want := int64(unix.ST_NOSUID | unix.ST_NODEV); int64(stat.Flags)&want != want {
		return errors.New("the volume cache is not on a nosuid,nodev mount")
	}
	return nil
}
