package codedaemon

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInputLimits(t *testing.T) {
	d := testDaemon(t, Config{Limits: Limits{InputBytes: 3, Files: 2}})
	for name, tc := range map[string]struct {
		files map[string]string
		want  error
	}{
		"exact":            {map[string]string{"bin/0": "abc"}, nil},
		"exact with empty": {map[string]string{"a": "abc", "b": ""}, nil},
		"bytes":            {map[string]string{"bin/0": "abcd"}, errInputLimit},
		"aggregate bytes":  {map[string]string{"a": "ab", "b": "cd"}, errInputLimit},
		"count":            {map[string]string{"a": "a", "b": "b", "c": ""}, errInputLimit},
	} {
		t.Run(name, func(t *testing.T) {
			r := taskRequest(t, `{"taskId":"test"}`, tc.files)
			_, mr := d.admit(httptest.NewRecorder(), r)
			if mr == nil {
				t.Fatal("request not admitted")
			}
			defer d.releaseLane()
			_, err := d.stageInputs(mr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("stage error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestInputDirectoryLimit(t *testing.T) {
	d := testDaemon(t, Config{Limits: Limits{InputBytes: 1, Files: 10}})
	// Each file creates its own chain of 501 directories, so 8 files fit
	// within maxDirs and 9 files do not.
	deepFiles := func(n int) map[string]string {
		files := make(map[string]string, n)
		for i := range n {
			files[strconv.Itoa(i)+"/"+strings.Repeat("d/", 500)+"f"] = ""
		}
		return files
	}
	for _, tc := range []struct {
		files int
		want  error
	}{{8, nil}, {9, errInputLimit}} {
		t.Run(strconv.Itoa(tc.files), func(t *testing.T) {
			_, mr := d.admit(httptest.NewRecorder(), taskRequest(t, `{"taskId":"test"}`, deepFiles(tc.files)))
			if mr == nil {
				t.Fatal("request not admitted")
			}
			defer d.releaseLane()
			dir, err := d.stageInputs(mr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("stage error = %v, want %v", err, tc.want)
			}
			if err == nil {
				_ = os.RemoveAll(dir)
			}
		})
	}
}

func TestOutputDirectoriesDontUseFileLimit(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "d", "e", "f")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "result.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if files, err := collectOutputs(root, Limits{OutputBytes: 1, Files: 1}); err != nil || len(files) != 1 {
		t.Fatalf("nested output: %v, %v", files, err)
	}
}

func TestOutputDirectoryLimit(t *testing.T) {
	dir := t.TempDir()
	for i := range maxDirs + 1 {
		if err := os.Mkdir(filepath.Join(dir, strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := collectOutputs(root, Limits{OutputBytes: 1, Files: 1}); !errors.Is(err, errOutputLimit) {
		t.Fatalf("directory limit error = %v", err)
	}
}

func TestFilePathLength(t *testing.T) {
	long := strings.Repeat("a/", 511) + "ab" // 1024 bytes
	if !validFilePath(long) || validFilePath(long+"c") {
		t.Fatal("paths must be valid up to 1024 bytes and invalid beyond")
	}
}

func TestOutputLimits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := collectOutputs(root, Limits{OutputBytes: 2, Files: 1}); !errors.Is(err, errOutputLimit) {
		t.Fatalf("output limit error = %v", err)
	}
	if files, err := collectOutputs(root, Limits{OutputBytes: 3, Files: 1}); err != nil || len(files) != 1 || files[0].Size != 3 {
		t.Fatalf("exact output limit: %v, %v", files, err)
	}
	if err := os.Symlink("result.json", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectOutputs(root, Limits{OutputBytes: 100, Files: 10}); !errors.Is(err, errInvalidOutput) {
		t.Fatalf("symlink output error = %v, want %v", err, errInvalidOutput)
	}
}
