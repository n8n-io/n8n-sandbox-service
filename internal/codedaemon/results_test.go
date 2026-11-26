package codedaemon

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestUserErrorInlinesResult(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"user-error","timeoutMs":60000}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted, terminal decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	if err := dec.Decode(&terminal); err != nil || terminal.Type != "error" || terminal.Code != "user_error" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
	if string(terminal.UserError) != `{"message":"boom","timeoutMs":60000}` || string(terminal.Result) != `{"console":["before throw"]}` {
		t.Fatalf("user error = %s, result = %s", terminal.UserError, terminal.Result)
	}
}

func TestDoneWithoutOutputsHasEmptyManifest(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"no-outputs","timeoutMs":60000}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	var terminal map[string]json.RawMessage
	if err := dec.Decode(&terminal); err != nil || string(terminal["type"]) != `"done"` {
		t.Fatalf("terminal = %v, %v", terminal, err)
	}
	if string(terminal["outputs"]) != `[]` || string(terminal["result"]) != `null` {
		t.Fatalf("outputs = %s, result = %s", terminal["outputs"], terminal["result"])
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
