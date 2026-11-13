package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckKernelAccess(t *testing.T) {
	tests := []struct {
		name    string
		uid     uint32
		mode    os.FileMode
		wantErr bool
	}{
		{"root-owned and read-only", 0, 0o444, false},
		{"writable by root alone", 0, 0o644, false},
		{"owned by the uid Firecracker runs as", 1000, 0o444, true},
		{"group-writable", 0, 0o464, true},
		{"world-writable", 0, 0o446, true},
		{"not readable by the uid Firecracker runs as", 0, 0o440, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKernelAccess("/srv/firecracker/template/vmlinux", tc.uid, tc.mode)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkKernelAccess(uid %d, mode %04o) error = %v, want error: %t", tc.uid, tc.mode, err, tc.wantErr)
			}
		})
	}
}

// World-writable, so the kernel is refused whichever user runs the test.
func TestCheckTemplateKernelRefusesAKernelOthersCanWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(path, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	err := checkTemplateKernel(path)
	if err == nil {
		t.Fatal("checkTemplateKernel() accepted a world-writable kernel")
	}
	for _, want := range []string{path, "mode 0666", "build-rootfs-template.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkTemplateKernel() error = %v, want it to contain %q", err, want)
		}
	}
}

// The cold boot is what reads the kernel, so one handed to another user after
// admission is refused there, although its size and modification time still match.
func TestVerifyKernelPinRefusesAKernelOthersCanWrite(t *testing.T) {
	rt := testRuntime(1)
	if err := rt.verifyKernelPin(testKernelPin); err != nil {
		t.Fatalf("verifyKernelPin() failed for an unchanged kernel: %v", err)
	}

	rt.deps.checkTemplateKernel = func(path string) error {
		return checkKernelAccess(path, 1000, 0o644)
	}
	if err := rt.verifyKernelPin(testKernelPin); err == nil || !strings.Contains(err.Error(), "owned by uid 1000") {
		t.Fatalf("verifyKernelPin() error = %v, want the kernel's owner refused", err)
	}
}
