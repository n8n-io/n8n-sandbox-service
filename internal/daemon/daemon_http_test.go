package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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
	handler := NewHandler(DefaultExecConfig())

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

	NewHandler(DefaultExecConfig()).ServeHTTP(rr, req)

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
	handler := NewHandler(DefaultExecConfig())

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
	handler := NewHandler(DefaultExecConfig())

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
	handler := NewHandler(DefaultExecConfig())
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
	handler := NewHandler(DefaultExecConfig())
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

	if code, body := get(dir); code != http.StatusBadRequest || !strings.Contains(string(body), "file_read read "+dir) {
		t.Fatalf("directory: expected 400, got %d: %s", code, body)
	}

	// A recorder keeps what a real server drops on HEAD, so it shows whether the file was read.
	head := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/files/content?path="+path, nil))
		return rr
	}
	if rr := head(file); rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: expected 200 and no bytes read, got %d and %d bytes", rr.Code, rr.Body.Len())
	}
	if rr := head(dir); rr.Code != http.StatusBadRequest {
		t.Fatalf("HEAD of a directory: expected 400 as for GET, got %d", rr.Code)
	}
}

func TestFileEndpointsRejectDirectoryMismatch(t *testing.T) {
	handler := NewHandler(DefaultExecConfig())
	base := tempBaseDir(t)
	directory := filepath.Join(base, "subdir")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	regularFile := filepath.Join(base, "file.txt")
	if err := os.WriteFile(regularFile, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	nestedUnderFile := filepath.Join(regularFile, "child")
	archiveBody := buildTar(t, tarFile("entry.txt", "data"))
	copyFileToDirectory, err := json.Marshal(copyRequest{Src: regularFile, Dest: directory, Overwrite: true})
	if err != nil {
		t.Fatalf("marshal copy request: %v", err)
	}
	moveDirectoryToFile, err := json.Marshal(moveRequest{Src: directory, Dest: regularFile, Overwrite: true})
	if err != nil {
		t.Fatalf("marshal move request: %v", err)
	}

	cases := []struct {
		name   string
		method string
		target string
		body   []byte
	}{
		{"PUT file over directory", http.MethodPut, "/files?path=" + directory, []byte("data")},
		{"POST append to directory", http.MethodPost, "/files?path=" + directory, []byte("data")},
		{"GET list regular file", http.MethodGet, "/files?path=" + regularFile, nil},
		{"GET content under regular file", http.MethodGet, "/files/content?path=" + nestedUnderFile, nil},
		{"PUT file under regular file", http.MethodPut, "/files?path=" + nestedUnderFile, []byte("data")},
		{"POST archive into regular file", http.MethodPost, "/files/archive?path=" + regularFile, archiveBody},
		{"POST copy file over directory", http.MethodPost, "/files/copy", copyFileToDirectory},
		{"POST move directory over file", http.MethodPost, "/files/move", moveDirectoryToFile},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.target, bytes.NewReader(testCase.body))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// failingWriter refuses every write once limit bytes have passed.
type failingWriter struct {
	http.ResponseWriter
	limit int
}

func (fw *failingWriter) Write(data []byte) (int, error) {
	if len(data) > fw.limit {
		return 0, errors.New("write failed")
	}
	fw.limit -= len(data)
	return fw.ResponseWriter.Write(data)
}

func TestFileContentEndpointCutsAFailedDownload(t *testing.T) {
	handler := NewHandler(DefaultExecConfig())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handler.ServeHTTP(&failingWriter{ResponseWriter: writer, limit: 1 << 16}, request)
	}))
	t.Cleanup(server.Close)

	file := filepath.Join(tempBaseDir(t), "data.bin")
	if err := os.WriteFile(file, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resp, err := http.Get(server.URL + "/files/content?path=" + file)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// The response has no Content-Length: the missing end of the chunked body tells the client.
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("expected the read to fail, got a complete response of %d bytes", len(body))
	}
}

func TestExecEndpointAppliesDefaultTimeout(t *testing.T) {
	oldTimeout := defaultExecTimeout
	defaultExecTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		defaultExecTimeout = oldTimeout
	})

	handler := NewHandler(DefaultExecConfig())
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
