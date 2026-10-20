package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckBridgeNetfilter(t *testing.T) {
	tests := []struct {
		name    string
		content *string
		wantErr string
	}{
		{name: "enabled", content: new("1\n")},
		{name: "disabled", content: new("0\n"), wantErr: `is "0", want "1"`},
		{name: "empty", content: new(""), wantErr: `is "", want "1"`},
		{name: "module not loaded", wantErr: "load the br_netfilter kernel module"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge-nf-call-iptables")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := checkBridgeNetfilter(path)
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
