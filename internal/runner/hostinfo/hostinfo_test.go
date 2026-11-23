package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMemTotal(t *testing.T) {
	got, err := MemTotal(write(t, "MemFree:  1 kB\nMemTotal:       65536 kB\n"))
	if err != nil || got != 65536*1024 {
		t.Fatalf("got %d, %v", got, err)
	}
	for _, bad := range []string{"", "MemTotal: abc kB\n", "MemTotal: 5 MB\n", "MemTotal: 0 kB\n"} {
		if _, err := MemTotal(write(t, bad)); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if _, err := MemTotal(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: want error")
	}
}

func TestVolumeTotal(t *testing.T) {
	if n, err := VolumeTotal(t.TempDir()); err != nil || n <= 0 {
		t.Fatalf("got %d, %v", n, err)
	}
}
