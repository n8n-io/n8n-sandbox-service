package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubLstat returns a fake lstatFn that reports every path as a regular directory
// unless the path is in the symlinks map, in which case it reports ModeSymlink.
// Paths in the notExist map return os.ErrNotExist.
func stubLstat(symlinks map[string]bool, notExist map[string]bool) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if notExist[path] {
			return nil, os.ErrNotExist
		}
		if symlinks[path] {
			return fakeFileInfo{name: filepath.Base(path), mode: os.ModeSymlink}, nil
		}
		return fakeFileInfo{name: filepath.Base(path), mode: os.ModeDir}, nil
	}
}

// stubEvalSymlinks returns an evalSymlinksFn that maps specific paths or passes through.
func stubEvalSymlinks(mapping map[string]string, notExist map[string]bool) func(string) (string, error) {
	return func(path string) (string, error) {
		if notExist[path] {
			return "", os.ErrNotExist
		}
		if resolved, ok := mapping[path]; ok {
			return resolved, nil
		}
		return path, nil
	}
}

// fakeFileInfo satisfies os.FileInfo for test stubs.
type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// withStubs sets the package-level lstatFn and evalSymlinksFn for the duration
// of the test and restores them on cleanup.
func withStubs(t *testing.T, lstat func(string) (os.FileInfo, error), eval func(string) (string, error)) {
	t.Helper()
	origLstat := lstatFn
	origEval := evalSymlinksFn
	lstatFn = lstat
	evalSymlinksFn = eval
	t.Cleanup(func() {
		lstatFn = origLstat
		evalSymlinksFn = origEval
	})
}

func TestSafeResolve_BasicPaths(t *testing.T) {
	base := "/sandbox"
	noSymlinks := stubLstat(nil, nil)
	identity := stubEvalSymlinks(nil, nil)

	tests := []struct {
		name     string
		userPath string
		want     string
	}{
		{"relative simple", "foo.txt", "/sandbox/foo.txt"},
		{"relative nested", "a/b/c.txt", "/sandbox/a/b/c.txt"},
		{"absolute in sandbox", "/home/user/file.txt", "/sandbox/home/user/file.txt"},
		{"with dot segments", "a/../b/c.txt", "/sandbox/b/c.txt"},
		{"with dot slash", "./a/b.txt", "/sandbox/a/b.txt"},
		{"trailing slash", "dir/", "/sandbox/dir"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withStubs(t, noSymlinks, identity)
			got, err := SafeResolve(base, tt.userPath)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeResolve_RootBase(t *testing.T) {
	// When daemon runs inside bwrap, base is "/".
	withStubs(t,
		stubLstat(nil, nil),
		stubEvalSymlinks(nil, nil),
	)

	got, err := SafeResolve("/", "/home/user/workspace/src/workflow.ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/home/user/workspace/src/workflow.ts" {
		t.Errorf("got %q, want %q", got, "/home/user/workspace/src/workflow.ts")
	}
}

func TestSafeResolve_BaseItself(t *testing.T) {
	withStubs(t,
		stubLstat(nil, nil),
		stubEvalSymlinks(nil, nil),
	)

	got, err := SafeResolve("/sandbox", ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/sandbox" {
		t.Errorf("got %q, want %q", got, "/sandbox")
	}
}

func TestSafeResolve_PathEscape(t *testing.T) {
	withStubs(t,
		stubLstat(nil, nil),
		stubEvalSymlinks(nil, nil),
	)

	tests := []struct {
		name     string
		userPath string
	}{
		{"dot-dot escape", "../../etc/passwd"},
		{"absolute dot-dot", "/../../../etc/shadow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SafeResolve("/sandbox", tt.userPath)
			if err == nil {
				t.Fatal("expected error for path escape")
			}
			if !errors.Is(err, ErrPathEscape) {
				t.Errorf("expected ErrPathEscape, got: %v", err)
			}
		})
	}
}

func TestSafeResolve_SymlinkDetected(t *testing.T) {
	symlinks := map[string]bool{
		"/sandbox/home/evil": true,
	}
	withStubs(t,
		stubLstat(symlinks, nil),
		stubEvalSymlinks(nil, nil),
	)

	_, err := SafeResolve("/sandbox", "/home/evil/file.txt")
	if err == nil {
		t.Fatal("expected error for symlink")
	}
	if !errors.Is(err, ErrSymlink) {
		t.Errorf("expected ErrSymlink, got: %v", err)
	}
}

func TestSafeResolve_SymlinkInMiddleComponent(t *testing.T) {
	symlinks := map[string]bool{
		"/sandbox/a/b": true,
	}
	withStubs(t,
		stubLstat(symlinks, nil),
		stubEvalSymlinks(nil, nil),
	)

	_, err := SafeResolve("/sandbox", "a/b/c/d.txt")
	if err == nil {
		t.Fatal("expected error for symlink in middle component")
	}
	if !errors.Is(err, ErrSymlink) {
		t.Errorf("expected ErrSymlink, got: %v", err)
	}
}

func TestSafeResolve_NonExistentPathAllowed(t *testing.T) {
	// First component exists, second doesn't — should succeed (for writes).
	notExist := map[string]bool{
		"/sandbox/newdir": true,
	}
	withStubs(t,
		stubLstat(nil, notExist),
		stubEvalSymlinks(nil, map[string]bool{"/sandbox/newdir/file.txt": true}),
	)

	got, err := SafeResolve("/sandbox", "newdir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/sandbox/newdir/file.txt" {
		t.Errorf("got %q, want %q", got, "/sandbox/newdir/file.txt")
	}
}

func TestSafeResolve_EvalSymlinksEscapeDetected(t *testing.T) {
	// All lstat checks pass (no symlinks), but evalSymlinks reveals the
	// resolved path actually escapes the base.
	mapping := map[string]string{
		"/sandbox/tricky": "/outside/escape",
	}
	withStubs(t,
		stubLstat(nil, nil),
		stubEvalSymlinks(mapping, nil),
	)

	_, err := SafeResolve("/sandbox", "tricky")
	if err == nil {
		t.Fatal("expected error for evalSymlinks escape")
	}
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("expected ErrPathEscape, got: %v", err)
	}
}

func TestSafeResolve_LstatError(t *testing.T) {
	errDisk := errors.New("disk I/O error")
	withStubs(t,
		func(path string) (os.FileInfo, error) {
			return nil, errDisk
		},
		stubEvalSymlinks(nil, nil),
	)

	_, err := SafeResolve("/sandbox", "file.txt")
	if err == nil {
		t.Fatal("expected error from lstat failure")
	}
	if !errors.Is(err, errDisk) {
		t.Errorf("expected wrapped disk error, got: %v", err)
	}
}

func TestSafeResolve_EvalSymlinksError(t *testing.T) {
	evalErr := errors.New("eval failure")
	withStubs(t,
		stubLstat(nil, nil),
		func(path string) (string, error) {
			return "", evalErr
		},
	)

	_, err := SafeResolve("/sandbox", "file.txt")
	if err == nil {
		t.Fatal("expected error from evalSymlinks failure")
	}
	if !errors.Is(err, evalErr) {
		t.Errorf("expected wrapped eval error, got: %v", err)
	}
}

func TestSafeResolve_RealFilesystem(t *testing.T) {
	// Integration test using real filesystem.
	base := tempBaseDir(t)

	// Create a directory and file.
	dir := filepath.Join(base, "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Create a symlink.
	linkPath := filepath.Join(base, "link")
	if err := os.Symlink(dir, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	t.Run("valid path", func(t *testing.T) {
		got, err := SafeResolve(base, "subdir/test.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != filePath {
			t.Errorf("got %q, want %q", got, filePath)
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		_, err := SafeResolve(base, "link/test.txt")
		if err == nil {
			t.Fatal("expected error for symlink")
		}
		if !errors.Is(err, ErrSymlink) {
			t.Errorf("expected ErrSymlink, got: %v", err)
		}
	})

	t.Run("escape rejected", func(t *testing.T) {
		_, err := SafeResolve(base, "../../etc/passwd")
		if err == nil {
			t.Fatal("expected error for escape")
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Errorf("expected ErrPathEscape, got: %v", err)
		}
	})

	t.Run("nonexistent path allowed", func(t *testing.T) {
		got, err := SafeResolve(base, "newfile.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(base, "newfile.txt")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
