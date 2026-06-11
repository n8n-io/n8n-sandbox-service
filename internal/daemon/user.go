package daemon

import (
	"fmt"
	"os"
	"syscall"
)

const (
	sandboxUID  = 1000
	sandboxGID  = 1000
	sandboxHome = "/home/user"
)

func PrepareSandboxUser() error {
	if os.Geteuid() != 0 {
		return nil
	}

	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(sandboxGID); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(sandboxUID); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if err := os.Setenv("HOME", sandboxHome); err != nil {
		return fmt.Errorf("set HOME: %w", err)
	}
	return nil
}

func commandSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{
			Uid: sandboxUID,
			Gid: sandboxGID,
		}
	}
	return attr
}
