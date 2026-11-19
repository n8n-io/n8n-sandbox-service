package hostinfo

import (
	"os"
	"path/filepath"
	"strconv"
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

func TestCgroupMemoryLimit(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel, "memory.max"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("runner.slice", "8589934592\n")
	mk("", "max\n")
	mk("capped", "junk")

	// The process cgroup wins over the root file.
	if n, ok, err := CgroupMemoryLimit(write(t, "0::/runner.slice\n"), root); err != nil || !ok || n != 8<<30 {
		t.Fatalf("got %d %v %v", n, ok, err)
	}
	// A leaf of "max" does not hide a limit on a parent. The tighter of the
	// two finite values wins.
	mk("system.slice/runner.service", "max")
	mk("system.slice", strconv.FormatInt(8<<30, 10))
	if n, ok, err := CgroupMemoryLimit(write(t, "0::/system.slice/runner.service\n"), root); err != nil || !ok || n != 8<<30 {
		t.Fatalf("parent limit: got %d %v %v", n, ok, err)
	}
	mk("tight.slice/runner.service", strconv.FormatInt(2<<30, 10))
	mk("tight.slice", strconv.FormatInt(8<<30, 10))
	if n, ok, err := CgroupMemoryLimit(write(t, "0::/tight.slice/runner.service\n"), root); err != nil || !ok || n != 2<<30 {
		t.Fatalf("tighter leaf: got %d %v %v", n, ok, err)
	}
	// 0::/ is the root file, here "max".
	if _, ok, err := CgroupMemoryLimit(write(t, "0::/\n"), root); err != nil || ok {
		t.Fatalf("root max: ok=%v err=%v", ok, err)
	}
	for name, self := range map[string]string{
		"no 0:: line (v1)": "12:memory:/x\n",
		"dotdot":           "0::/../etc\n",
		"missing file":     "0::/nope\n",
	} {
		if _, ok, err := CgroupMemoryLimit(write(t, self), root); err != nil || ok {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
	if _, ok, err := CgroupMemoryLimit(filepath.Join(t.TempDir(), "missing"), root); err != nil || ok {
		t.Errorf("no self file: ok=%v err=%v", ok, err)
	}
	if _, _, err := CgroupMemoryLimit(write(t, "0::/capped\n"), root); err == nil {
		t.Error("junk value: want error")
	}
}

func TestVolumeTotal(t *testing.T) {
	if n, err := VolumeTotal(t.TempDir()); err != nil || n <= 0 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestMemoryBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "memory.max"), []byte("4294967296\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meminfo := write(t, "MemTotal: 67108864 kB\n") // 64 GiB
	if got, err := MemoryBudget(meminfo, write(t, "0::/\n"), root); err != nil || got != 4<<30 {
		t.Fatalf("lower cgroup limit: got %d, %v", got, err)
	}
	if got, err := MemoryBudget(meminfo, filepath.Join(t.TempDir(), "missing"), root); err != nil || got != 64<<30 {
		t.Fatalf("no cgroup: got %d, %v", got, err)
	}
}
