package codedaemon

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func inputParts(t *testing.T, paths ...string) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, path := range paths {
		part, err := mw.CreateFormFile("input", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return multipart.NewReader(&body, mw.Boundary())
}

func TestStageInputs(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		ok    bool
	}{
		{name: "nested files", paths: []string{"bin/0", "bin/1", "a/b/c.txt"}, ok: true},
		{name: "parent escape", paths: []string{"../x"}},
		{name: "nested parent escape", paths: []string{"a/../../x"}},
		{name: "absolute", paths: []string{"/abs"}},
		{name: "backslash", paths: []string{`a\b`}},
		{name: "empty element", paths: []string{"a//b"}},
		{name: "dot", paths: []string{"."}},
		{name: "empty", paths: []string{""}},
		{name: "duplicate", paths: []string{"bin/0", "bin/0"}},
		{name: "file then directory", paths: []string{"a", "a/b"}},
		{name: "directory then file", paths: []string{"a/b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDaemon(t, Config{})
			dir, err := d.stageInputs(inputParts(t, tt.paths...))
			if !tt.ok {
				if err == nil {
					t.Fatal("upload was accepted")
				}
				if entries, err := os.ReadDir(d.cfg.WorkDir); err != nil || len(entries) != 0 {
					t.Fatalf("rejected upload left %v in the work dir (%v)", entries, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range tt.paths {
				b, err := os.ReadFile(filepath.Join(dir, "in", path))
				if err != nil || string(b) != path {
					t.Fatalf("%s: got %q (%v)", path, b, err)
				}
			}
		})
	}
}

func TestStageInputsRejectsUploadWithoutClosingBoundary(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("input", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "a"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	cut := bytes.TrimSuffix(body.Bytes(), []byte("--\r\n"))
	d := testDaemon(t, Config{})
	if _, err := d.stageInputs(multipart.NewReader(bytes.NewReader(cut), mw.Boundary())); err == nil {
		t.Fatal("upload without the closing boundary was accepted")
	}
}

func TestStageInputsRejectsNonInputPart(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("spec", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	d := testDaemon(t, Config{})
	if _, err := d.stageInputs(multipart.NewReader(&body, mw.Boundary())); err == nil {
		t.Fatal("non-input part was accepted")
	}
}

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
			r := taskRequest(t, `{"taskId":"test","timeoutMs":60000}`, tc.files)
			spec, mr := d.admit(httptest.NewRecorder(), r)
			if mr == nil {
				t.Fatal("request not admitted")
			}
			defer d.releaseLane()
			defer d.releaseTask(spec.TaskID)
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
			spec, mr := d.admit(httptest.NewRecorder(), taskRequest(t, `{"taskId":"test","timeoutMs":60000}`, deepFiles(tc.files)))
			if mr == nil {
				t.Fatal("request not admitted")
			}
			defer d.releaseLane()
			defer d.releaseTask(spec.TaskID)
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

func TestFilePathLength(t *testing.T) {
	long := strings.Repeat("a/", 511) + "ab" // 1024 bytes
	if !validFilePath(long) || validFilePath(long+"c") {
		t.Fatal("paths must be valid up to 1024 bytes and invalid beyond")
	}
}
