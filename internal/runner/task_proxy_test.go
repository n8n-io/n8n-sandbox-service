package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/codetask"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

const taskTestContentType = "multipart/form-data; boundary=b"

// The route reaches the daemon's /tasks with the multipart Content-Type, whose
// boundary the daemon needs to read the parts.
func TestRunnerProxiesTaskToDaemon(t *testing.T) {
	var gotPath, gotContentType, gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotContentType = r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"done"}` + "\n"))
	}))
	defer daemon.Close()

	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}, MaxTaskInputBytes: 1024}
	router := NewRouter(&fakeRuntime{daemonURL: daemon.URL}, cfg, metrics.NewRunnerRecorder(false))

	req := authedRequest(context.Background(), http.MethodPost, "/sandboxes/"+proxyTestSandboxID+"/tasks", "parts")
	req.Header.Set("Content-Type", taskTestContentType)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != `{"type":"done"}`+"\n" {
		t.Fatalf("got %d %q, want the daemon's events", rr.Code, rr.Body.String())
	}
	if gotPath != "/tasks" {
		t.Errorf("daemon path = %q, want /tasks", gotPath)
	}
	if gotContentType != taskTestContentType {
		t.Errorf("daemon Content-Type = %q, want %q", gotContentType, taskTestContentType)
	}
	if gotBody != "parts" {
		t.Errorf("daemon body = %q, want parts", gotBody)
	}
}

// The body may exceed the input limit by the room for the spec and the
// multipart headers, and no more.
func TestRunnerCapsTaskBody(t *testing.T) {
	maxBody := codetask.MaxBodyBytes(1024)
	for _, tc := range []struct {
		size int64
		want int
	}{
		{maxBody, http.StatusOK},
		{maxBody + 1, http.StatusBadRequest},
	} {
		var received atomic.Int64
		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			received.Add(n)
			w.WriteHeader(http.StatusOK)
		}))
		defer daemon.Close()

		cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}, MaxFileBytes: 1 << 20, MaxTaskInputBytes: 1024}
		router := NewRouter(&fakeRuntime{daemonURL: daemon.URL}, cfg, metrics.NewRunnerRecorder(false))

		req := authedRequest(context.Background(), http.MethodPost, "/sandboxes/"+proxyTestSandboxID+"/tasks", strings.Repeat("x", int(tc.size)))
		req.Header.Set("Content-Type", taskTestContentType)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != tc.want {
			t.Fatalf("%d-byte body: status = %d, want %d: %s", tc.size, rr.Code, tc.want, rr.Body.String())
		}
		if n := received.Load(); n > maxBody {
			t.Fatalf("%d-byte body: daemon received %d bytes, past the %d-byte cap", tc.size, n, maxBody)
		}
	}
}

// Neither direction is buffered: the daemon reads the first part of the upload
// while the client still holds the rest, and the client reads the first event
// while the daemon still holds the last.
func TestRunnerStreamsTaskBothWays(t *testing.T) {
	firstPartRead := make(chan struct{})
	firstEventRead := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len("first"))
		if _, err := io.ReadFull(r.Body, buf); err != nil || string(buf) != "first" {
			t.Errorf("first part = %q, %v", buf, err)
			return
		}
		close(firstPartRead)
		if rest, _ := io.ReadAll(r.Body); string(rest) != "rest" {
			t.Errorf("rest = %q, want rest", rest)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"accepted"}` + "\n"))
		w.(http.Flusher).Flush()
		select {
		case <-firstEventRead:
		case <-time.After(5 * time.Second):
			t.Error("the first event did not reach the client before the stream ended")
		}
		_, _ = w.Write([]byte(`{"type":"done"}` + "\n"))
	}))
	defer daemon.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/tasks", TaskProxyHandler(&fakeRuntime{daemonURL: daemon.URL}, proxyTestConfig(), metrics.NewRunnerRecorder(false)))
	runner := httptest.NewServer(mux)
	defer runner.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("first"))
		select {
		case <-firstPartRead:
		case <-time.After(5 * time.Second):
			t.Error("the first part did not reach the daemon before the upload ended")
		}
		_, _ = pw.Write([]byte("rest"))
		_ = pw.Close()
	}()

	resp, err := http.Post(runner.URL+"/sandboxes/"+proxyTestSandboxID+"/tasks", taskTestContentType, pr)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	lines := bufio.NewReader(resp.Body)
	first, err := lines.ReadString('\n')
	if err != nil || first != `{"type":"accepted"}`+"\n" {
		t.Fatalf("first event = %q, %v", first, err)
	}
	close(firstEventRead)
	last, err := lines.ReadString('\n')
	if err != nil || last != `{"type":"done"}`+"\n" {
		t.Fatalf("last event = %q, %v", last, err)
	}
}

// A daemon that refuses a task before the upload ends, as its admission does,
// gets its answer to the client while the client still holds the upload open.
func TestRunnerRelaysEarlyTaskRejection(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		buf := make([]byte, len("spec"))
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			t.Errorf("read spec: %v", err)
		}
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		http.Error(w, "no free lane", http.StatusTooManyRequests)
	}))
	defer daemon.Close()

	handlerDone := make(chan struct{})
	task := TaskProxyHandler(&fakeRuntime{daemonURL: daemon.URL}, proxyTestConfig(), metrics.NewRunnerRecorder(false))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		task(w, r)
	})
	runner := httptest.NewServer(mux)
	t.Cleanup(runner.Close)

	resp := postUnfinishedUpload(t, runner.Listener.Addr().String(), "/sandboxes/"+proxyTestSandboxID+"/tasks", "spec")
	if resp.StatusCode != http.StatusTooManyRequests || !resp.Close {
		t.Fatalf("status = %d, close = %v; want 429 and close", resp.StatusCode, resp.Close)
	}
	// The handler holds the sandbox's request slot until it returns.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return while the upload stayed open")
	}
}

// postUnfinishedUpload sends a chunked POST with body and leaves the upload
// open, then returns the response, failing if none comes within 5 seconds.
func postUnfinishedUpload(t *testing.T, addr, path, body string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last first, so this closes the connection before a server
	// registered earlier waits for it.
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", path, taskTestContentType, len(body), body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
