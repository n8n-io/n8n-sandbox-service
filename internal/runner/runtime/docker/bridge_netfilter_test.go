package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckBridgeNetfilter(t *testing.T) {
	tests := []struct {
		name           string
		content        *string
		release        *string
		ack            bool
		wantUnverified bool
		wantErr        string
	}{
		{name: "enabled", content: new("1\n")},
		{name: "disabled", content: new("0\n"), wantErr: `is "0", want "1"`},
		{name: "disabled, ack ignored", content: new("0\n"), release: new("4.4.302+\n"), ack: true, wantErr: `is "0", want "1"`},
		{name: "empty", content: new(""), wantErr: `is "", want "1"`},
		{name: "module not loaded", release: new("6.8.0-45-generic\n"), wantErr: "load the br_netfilter kernel module"},
		{name: "module not loaded on 5.3, ack ignored", release: new("5.3.0\n"), ack: true, wantErr: "load the br_netfilter kernel module"},
		{name: "pre-5.3 kernel, acked", release: new("4.4.302+\n"), ack: true, wantUnverified: true},
		{name: "pre-5.3 kernel, not acked", release: new("4.19.0-27-amd64\n"), wantErr: envAckBridgeNetfilter + "=true"},
		{name: "unrecognised release", release: new("weird\n"), ack: true, wantErr: `unrecognised kernel release "weird"`},
		{name: "release unreadable", ack: true, wantErr: "read kernel release"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := bridgeNetfilterPaths{
				callIPTables: filepath.Join(dir, "bridge-nf-call-iptables"),
				osRelease:    filepath.Join(dir, "osrelease"),
			}
			if tt.content != nil {
				if err := os.WriteFile(p.callIPTables, []byte(*tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.release != nil {
				if err := os.WriteFile(p.osRelease, []byte(*tt.release), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			unverified, err := checkBridgeNetfilter(p, tt.ack)
			if unverified != tt.wantUnverified {
				t.Fatalf("checkBridgeNetfilter() unverified = %v, want %v", unverified, tt.wantUnverified)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkBridgeNetfilter() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkBridgeNetfilter() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestKernelAtLeast(t *testing.T) {
	tests := []struct {
		release     string
		wantAtLeast bool
		wantOK      bool
	}{
		{release: "4.4.302+", wantOK: true},
		{release: "4.19.0-27-amd64", wantOK: true},
		{release: "5.2.21", wantOK: true},
		{release: "5.3.0", wantAtLeast: true, wantOK: true},
		{release: "5.3-rc1", wantAtLeast: true, wantOK: true},
		{release: "5.15.0-91-generic", wantAtLeast: true, wantOK: true},
		{release: "6.1.188", wantAtLeast: true, wantOK: true},
		{release: "3.10.0-1160.el7.x86_64", wantOK: true},
		{release: "5", wantOK: false},
		{release: "x.3", wantOK: false},
		{release: "5.x", wantOK: false},
		{release: "", wantOK: false},
	}
	for _, tt := range tests {
		atLeast, ok := kernelAtLeast(tt.release, 5, 3)
		if atLeast != tt.wantAtLeast || ok != tt.wantOK {
			t.Errorf("kernelAtLeast(%q, 5, 3) = %v, %v; want %v, %v", tt.release, atLeast, ok, tt.wantAtLeast, tt.wantOK)
		}
	}
}
