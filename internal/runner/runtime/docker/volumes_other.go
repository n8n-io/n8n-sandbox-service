//go:build !linux

package docker

import "errors"

var errVolumesNeedLinux = errors.New("volumes on Docker need a Linux runner")

func setNoSuidNoDev(string) error { return errVolumesNeedLinux }

func checkNoSuidNoDev(string) error { return errVolumesNeedLinux }
