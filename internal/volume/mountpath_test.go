package volume

import (
	"strings"
	"testing"
)

func TestValidateMountPath(t *testing.T) {
	longest := "/mnt/" + strings.Repeat("a", 255-len("/mnt/"))
	tests := []struct {
		name      string
		mountPath string
		wantErr   string
	}{
		{"below mnt", "/mnt/knowledge", ""},
		{"longest path", longest, ""},

		{"relative path", "mnt/a", "not absolute and clean"},
		{"empty path", "", "not absolute and clean"},
		{"dot-dot", "/mnt/a/../b", "not absolute and clean"},
		{"escape through dot-dot", "/mnt/../etc", "not absolute and clean"},
		{"double slash", "/mnt//a", "not absolute and clean"},
		{"trailing slash", "/mnt/a/", "not absolute and clean"},
		{"mnt itself", "/mnt", "not below"},
		{"below home", "/home/user/data/set", "not below"},
		{"mnt as a prefix", "/mntx/a", "not below"},
		{"system path", "/etc/cron.d", "not below"},
		{"path too long", longest + "a", "longer than 255 bytes"},
		{"NUL in path", "/mnt/a\x00b", "control character"},
		{"newline in path", "/mnt/a\nb", "control character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMountPath(tt.mountPath)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMountPath() error = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateMountPath() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMountPathsOverlap(t *testing.T) {
	tests := []struct {
		name          string
		first, second string
		want          bool
	}{
		{"same path", "/mnt/a", "/mnt/a", true},
		{"second inside the first", "/mnt/a", "/mnt/a/b", true},
		{"second around the first", "/mnt/a/b", "/mnt/a", true},
		{"siblings with a common prefix", "/mnt/a", "/mnt/ab", false},
		{"siblings with a common prefix, reversed", "/mnt/ab", "/mnt/a", false},
		{"same name in different directories", "/mnt/a/b", "/mnt/c/b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MountPathsOverlap(tt.first, tt.second); got != tt.want {
				t.Fatalf("MountPathsOverlap(%q, %q) = %v, want %v", tt.first, tt.second, got, tt.want)
			}
		})
	}
}
