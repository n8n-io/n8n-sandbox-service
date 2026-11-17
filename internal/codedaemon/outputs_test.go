package codedaemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadAndDeleteOutputsWithoutReusingTaskID(t *testing.T) {
	d := testDaemon(t, Config{})
	h := d.Handler()
	const spec = `{"taskId":"outputs","inlineResultMaxBytes":1}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, taskRequest(t, spec, nil))
	dec := json.NewDecoder(w.Body)
	var ack decodedEvent
	if err := dec.Decode(&ack); err != nil || ack.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", ack, err)
	}
	done := finishTask(t, dec, "")
	if done.OutputExpiresAt == nil || !done.OutputExpiresAt.After(time.Now()) || string(done.Result) != "null" || len(done.Outputs) != 1 {
		t.Fatalf("invalid outputs metadata: %+v", done)
	}
	const path = "/tasks/outputs/outputs/result.json"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"outputs"`) {
		t.Fatalf("download status %d, body %q", w.Code, w.Body.String())
	}
	for range 2 {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/tasks/outputs", nil))
		if w.Code != http.StatusNoContent {
			t.Fatalf("cleanup status = %d", w.Code)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("download after cleanup status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, taskRequest(t, spec, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate after cleanup status = %d", w.Code)
	}
}

func TestOutputExpiryLeavesOpenDownloadReadable(t *testing.T) {
	store := outputStore{tasks: make(map[string]*retainedOutputs), maxBytes: 3}
	defer store.close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	expires, ok := store.add("task", dir, root, []outputEntry{{Path: "result.json", Size: 3}}, 500*time.Millisecond)
	if !ok {
		root.Close()
		t.Fatal("could not retain outputs")
	}
	f, size := store.open("task", "result.json")
	if f == nil || size != 3 {
		t.Fatal("could not open download")
	}
	defer f.Close()
	<-time.After(time.Until(expires))
	if f, _ := store.open("task", "result.json"); f != nil {
		f.Close()
		t.Fatal("new download opened after expiry")
	}
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired files not removed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if data, err := io.ReadAll(f); err != nil || string(data) != "123" {
		t.Fatalf("open download interrupted: %q, %v", data, err)
	}
}
