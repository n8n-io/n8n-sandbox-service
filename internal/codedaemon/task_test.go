package codedaemon

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
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
