package daemon

import (
	"bufio"
	"bytes"
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

func TestExecEndpointStreamsResponses(t *testing.T) {
	handler := NewHandler()

	req := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader([]byte(`{"command":"echo hello","env":{"FOO":"bar"}}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	dec := json.NewDecoder(bufio.NewReader(bytes.NewReader(rr.Body.Bytes())))
	var sawStdout bool
	var sawExit bool
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		switch resp.Type {
		case ResponseTypeStdout:
			if resp.Data == "hello\n" {
				sawStdout = true
			}
		case ResponseTypeExit:
			sawExit = true
		}
	}

	if !sawStdout {
		t.Fatal("expected stdout response")
	}
	if !sawExit {
		t.Fatal("expected exit response")
	}
}

func TestExecEndpointKeepsMultibyteCharactersInLongLinesIntact(t *testing.T) {
	// The odd prefix puts a 2-byte character across every 64 KiB chunk boundary.
	line := "a" + strings.Repeat("é", 100_000)
	path := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"command": "cat " + shellQuote(path)})

	req := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	NewHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var stdout strings.Builder
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeStdout {
			stdout.WriteString(resp.Data)
		}
	}

	if stdout.String() != line+"\n" {
		t.Fatalf("stdout was altered: got %d bytes, expected %d", stdout.Len(), len(line)+1)
	}
}

func TestExecEndpointRejectsArrayEnv(t *testing.T) {
	handler := NewHandler()

	req := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader([]byte(`{"command":"echo hello","env":["FOO=bar"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "cannot unmarshal array") {
		t.Fatalf("expected array env unmarshal error, got: %s", rr.Body.String())
	}
}

func TestFileCopyRejectsOversizedJSONBody(t *testing.T) {
	handler := NewHandler()

	body := `{"src":"` + strings.Repeat("x", maxJSONBodyBytes) + `","dest":"/tmp/out"}`
	req := httptest.NewRequest(http.MethodPost, "/files/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFileArchiveEndpoint(t *testing.T) {
	handler := NewHandler()
	target := tempBaseDir(t)

	post := func(query string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/files/archive"+query, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-tar")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	rr := post("?path="+target, buildTar(t, tarFile("a.txt", "hello")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"files":1,"bytes":5}` {
		t.Fatalf("unexpected body: %s", got)
	}

	if rr := post("", buildTar(t, tarFile("a.txt", "hello"))); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing path: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = post("?path="+target, buildTar(t, tarFile("../a.txt", "x")))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `\"../a.txt\"`) {
		t.Fatalf("refused entry: expected 400 naming the entry, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFileContentEndpoint(t *testing.T) {
	// A real server: the directory case relies on net/http not sending the 200
	// before the first byte is read.
	handler := NewHandler()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dir := tempBaseDir(t)

	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(server.URL + "/files/content?path=" + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body of %s: %v", path, err)
		}
		return resp.StatusCode, body
	}

	content := bytes.Repeat([]byte("\x00\xffa\n"), 1<<16)
	file := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if code, body := get(file); code != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("file: expected 200 with %d unchanged bytes, got %d with %d bytes", len(content), code, len(body))
	}

	missing := filepath.Join(dir, "missing")
	if code, body := get(missing); code != http.StatusNotFound || !strings.Contains(string(body), "file_read open "+missing) {
		t.Fatalf("missing file: expected 404, got %d: %s", code, body)
	}

	if code, body := get(dir); code != http.StatusInternalServerError || !strings.Contains(string(body), "file_read read "+dir) {
		t.Fatalf("directory: expected 500, got %d: %s", code, body)
	}

	// A recorder keeps what a real server drops on HEAD, so it shows whether the file was read.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/files/content?path="+file, nil))
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: expected 200 and no bytes read, got %d and %d bytes", rr.Code, rr.Body.Len())
	}
}

func TestExecEndpointAppliesDefaultTimeout(t *testing.T) {
	oldTimeout := defaultExecTimeout
	defaultExecTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		defaultExecTimeout = oldTimeout
	})

	handler := NewHandler()
	req := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader([]byte(`{"command":"sleep 1"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	dec := json.NewDecoder(bufio.NewReader(bytes.NewReader(rr.Body.Bytes())))
	var exit *Response
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeExit {
			respCopy := resp
			exit = &respCopy
		}
	}

	if exit == nil {
		t.Fatal("expected exit response")
	}
	if exit.TimedOut == nil || !*exit.TimedOut {
		t.Fatalf("expected timed_out=true, got %+v", exit)
	}
}
