package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

func seq(n uint64) *uint64 { return &n }

func startedEvent(execID string) string {
	return fmt.Sprintf(`{"seq":0,"type":"started","exec_id":%s}`, strconv.Quote(execID))
}

func stdoutEvent(s uint64, data string) string {
	return fmt.Sprintf(`{"seq":%d,"type":"stdout","data":%s}`, s, strconv.Quote(data))
}

func exitEvent(s uint64) string {
	return fmt.Sprintf(`{"seq":%d,"type":"exit","exit_code":0,"success":true,"execution_time_ms":10,"timed_out":false,"killed":false}`, s)
}

func parseNDJSON(body []byte) []ndjsonEvent {
	var events []ndjsonEvent
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		var ev ndjsonEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events
}

func TestExecProxyHappyPath(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-happy"
	lines := strings.Join([]string{
		startedEvent(execID),
		stdoutEvent(1, "hello\n"),
		exitEvent(2),
	}, "\n") + "\n"

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(lines))
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseNDJSON(rec.Body.Bytes())
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %s", len(events), rec.Body.String())
	}
	if events[0].Type != "started" {
		t.Fatalf("expected started, got %s", events[0].Type)
	}
	if events[1].Type != "stdout" {
		t.Fatalf("expected stdout, got %s", events[1].Type)
	}
	if events[2].Type != "exit" {
		t.Fatalf("expected exit, got %s", events[2].Type)
	}

	if xid := rec.Header().Get("X-Exec-Id"); xid != execID {
		t.Fatalf("expected X-Exec-Id=%s, got %s", execID, xid)
	}
}

func TestExecProxyRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	var daemonCalls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemonCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := `{"command":"` + strings.Repeat("x", execMaxJSONBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("expected request body too large error, got %s", rec.Body.String())
	}
	if n := daemonCalls.Load(); n != 0 {
		t.Fatalf("expected daemon not to be called, got %d calls", n)
	}
}

func TestExecProxyStreamsLargeEvent(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-large-event"
	largeData := strings.Repeat("x", 128*1024)
	lines := strings.Join([]string{
		startedEvent(execID),
		stdoutEvent(1, largeData),
		exitEvent(2),
	}, "\n") + "\n"

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(lines))
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(largeData)) {
		t.Fatal("expected large stdout event to be forwarded")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"type":"exit"`)) {
		t.Fatal("expected exit event to be forwarded")
	}
}

func TestExecProxyOneRetry(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-retry"

	var getCalls atomic.Int32

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/executions" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(startedEvent(execID) + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Close the connection by hijacking — simulates TCP drop.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter does not support Hijack")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/executions/") {
			getCalls.Add(1)
			after := r.URL.Query().Get("after")
			if after != "0" {
				t.Errorf("expected after=0, got %s", after)
			}

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			lines := stdoutEvent(1, "hello\n") + "\n" + exitEvent(2) + "\n"
			w.Write([]byte(lines))
			return
		}
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseNDJSON(rec.Body.Bytes())
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %s", len(events), rec.Body.String())
	}
	if events[0].Type != "started" {
		t.Errorf("event[0]: expected started, got %s", events[0].Type)
	}
	if events[1].Type != "stdout" {
		t.Errorf("event[1]: expected stdout, got %s", events[1].Type)
	}
	if events[2].Type != "exit" {
		t.Errorf("event[2]: expected exit, got %s", events[2].Type)
	}

	if n := getCalls.Load(); n != 1 {
		t.Errorf("expected 1 resume GET call, got %d", n)
	}
}

func TestExecProxyResumeWithoutAfterBeforeFirstEvent(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-retry-before-started"

	var getCalls atomic.Int32

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/executions" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter does not support Hijack")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/executions/") {
			getCalls.Add(1)
			if _, ok := r.URL.Query()["after"]; ok {
				t.Errorf("expected resume before first event to omit after, got %s", r.URL.RawQuery)
			}

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			lines := strings.Join([]string{
				startedEvent(execID),
				stdoutEvent(1, "hello\n"),
				exitEvent(2),
			}, "\n") + "\n"
			w.Write([]byte(lines))
			return
		}
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseNDJSON(rec.Body.Bytes())
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %s", len(events), rec.Body.String())
	}
	if events[0].Type != "started" {
		t.Fatalf("expected first event to be started, got %s", events[0].Type)
	}
	if n := getCalls.Load(); n != 1 {
		t.Errorf("expected 1 resume GET call, got %d", n)
	}
}

func TestExecProxyRetriesExhausted(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-exhaust"

	var mu sync.Mutex
	var postCalls, getCalls int

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodPost {
			postCalls++
		} else {
			getCalls++
		}
		mu.Unlock()

		if r.Method == http.MethodPost && r.URL.Path == "/executions" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(startedEvent(execID) + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}

		// Resume GET also disconnects immediately.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseNDJSON(rec.Body.Bytes())
	if len(events) != 1 {
		t.Fatalf("expected 1 event (started only), got %d: %s", len(events), rec.Body.String())
	}
	if events[0].Type != "started" {
		t.Errorf("expected started event, got %s", events[0].Type)
	}

	mu.Lock()
	defer mu.Unlock()
	if getCalls < 1 {
		t.Errorf("expected at least 1 resume attempt, got %d", getCalls)
	}
}

func TestExecProxyExecIDPropagation(t *testing.T) {
	t.Parallel()

	t.Run("generated", func(t *testing.T) {
		t.Parallel()
		var receivedExecID string
		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body execRequestBody
			json.NewDecoder(r.Body).Decode(&body)
			receivedExecID = body.ExecID

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(startedEvent(body.ExecID) + "\n" + exitEvent(1) + "\n"))
		}))
		defer daemon.Close()

		handler := ExecProxyHandler(
			&fakeContainerManager{daemonURL: daemon.URL},
			&config.Config{},
		)

		req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(`{"command":"echo hi"}`))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		xid := rec.Header().Get("X-Exec-Id")
		if xid == "" {
			t.Fatal("expected X-Exec-Id header to be set")
		}
		if receivedExecID == "" {
			t.Fatal("expected daemon to receive exec_id")
		}
		if xid != receivedExecID {
			t.Fatalf("X-Exec-Id (%s) != daemon exec_id (%s)", xid, receivedExecID)
		}
	})

	t.Run("explicit", func(t *testing.T) {
		t.Parallel()
		const explicitID = "my-explicit-exec-id"
		var receivedExecID string

		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body execRequestBody
			json.NewDecoder(r.Body).Decode(&body)
			receivedExecID = body.ExecID

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(startedEvent(body.ExecID) + "\n" + exitEvent(1) + "\n"))
		}))
		defer daemon.Close()

		handler := ExecProxyHandler(
			&fakeContainerManager{daemonURL: daemon.URL},
			&config.Config{},
		)

		body := fmt.Sprintf(`{"command":"echo hi","exec_id":"%s"}`, explicitID)
		req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		xid := rec.Header().Get("X-Exec-Id")
		if xid != explicitID {
			t.Fatalf("expected X-Exec-Id=%s, got %s", explicitID, xid)
		}
		if receivedExecID != explicitID {
			t.Fatalf("expected daemon to receive exec_id=%s, got %s", explicitID, receivedExecID)
		}
	})
}

func TestExecProxyAlreadyCompletedOnResume(t *testing.T) {
	t.Parallel()
	const execID = "test-exec-completed"

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/executions" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(startedEvent(execID) + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/executions/") {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(exitEvent(1) + "\n"))
			return
		}
	}))
	defer daemon.Close()

	handler := ExecProxyHandler(
		&fakeContainerManager{daemonURL: daemon.URL},
		&config.Config{},
	)

	body := fmt.Sprintf(`{"command":"echo hello","exec_id":"%s"}`, execID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions", strings.NewReader(body))
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseNDJSON(rec.Body.Bytes())
	if len(events) != 2 {
		t.Fatalf("expected 2 events (started + exit), got %d: %s", len(events), rec.Body.String())
	}
	if events[0].Type != "started" {
		t.Errorf("event[0]: expected started, got %s", events[0].Type)
	}
	if events[1].Type != "exit" {
		t.Errorf("event[1]: expected exit, got %s", events[1].Type)
	}
}
