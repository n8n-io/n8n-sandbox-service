package codedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamFlushesThroughTerminal(t *testing.T) {
	d := testDaemon(t, Config{})
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	client := &http.Client{Timeout: 2 * heartbeatInterval}
	req := taskRequest(t, `{"taskId":"quiet"}`, nil)
	res, err := client.Post(srv.URL+"/tasks", req.Header.Get("Content-Type"), req.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("unexpected response: %s %v", res.Status, res.Header)
	}
	dec := json.NewDecoder(res.Body)
	for _, want := range []string{"accepted", "heartbeat", "done"} {
		var event decodedEvent
		if err := dec.Decode(&event); err != nil || event.Type != want || event.TaskID != "quiet" {
			t.Fatalf("event = %+v, %v; want %q", event, err, want)
		}
		if strings.Contains(string(event.Result), "raw-runtime-") {
			t.Fatalf("unexpected runtime logs in event: %+v", event)
		}
	}
	if err := dec.Decode(new(decodedEvent)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected exactly one terminal event, got %v", err)
	}
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"quiet"}`, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d", w.Code)
	}
}

func TestRejectedUploadReleasesLaneAndID(t *testing.T) {
	d := testDaemon(t, Config{Limits: Limits{InputBytes: 10}})
	h := d.Handler()
	for _, tc := range []struct {
		files  map[string]string
		status int
	}{
		{map[string]string{"bin/0": "01234567890"}, http.StatusRequestEntityTooLarge},
		{nil, http.StatusOK},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, taskRequest(t, `{"taskId":"retry"}`, tc.files))
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
}

type readDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *readDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestRejectedUploadKeepsReadDeadline(t *testing.T) {
	d := testDaemon(t, Config{})
	if err := d.reserveTask("duplicate"); err != nil {
		t.Fatal(err)
	}
	w := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"duplicate"}`, nil))
	if w.Code != http.StatusConflict || w.deadline.IsZero() {
		t.Fatalf("status = %d, read deadline = %v", w.Code, w.deadline)
	}
}

func TestCooperativeDisconnectDoesNotCancelSibling(t *testing.T) {
	d := testDaemon(t, Config{MaxConcurrency: 2})
	srv := httptest.NewServer(d.Handler())
	// On a test failure, unblock any detached handler before closing the server.
	defer srv.Close()
	defer d.Close()
	client := &http.Client{Timeout: 2 * heartbeatInterval}
	start := func(ctx context.Context, id string) *json.Decoder {
		t.Helper()
		upload := taskRequest(t, `{"taskId":"`+id+`"}`, nil)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/tasks", upload.Body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = upload.Header
		req.ContentLength = upload.ContentLength
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		dec := json.NewDecoder(res.Body)
		var event decodedEvent
		if err := dec.Decode(&event); err != nil || event.Type != "accepted" {
			t.Fatalf("first event = %+v, %v", event, err)
		}
		return dec
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start(ctx, "cooperative")
	other := start(t.Context(), "other")
	cancel()
	for {
		var event decodedEvent
		if err := other.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "heartbeat" {
			continue
		}
		if event.Type != "done" {
			t.Fatalf("other task's terminal event = %+v", event)
		}
		break
	}
}

// decodedEvent holds the fields of any task event, for tests that read a stream.
type decodedEvent struct {
	Type      string          `json:"type"`
	TaskID    string          `json:"taskId"`
	Result    json.RawMessage `json:"result"`
	Outputs   []outputEntry   `json:"outputs"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	UserError json.RawMessage `json:"userError"`

	OutputExpiresAt *time.Time `json:"outputExpiresAt"`
}

func TestUserErrorInlinesResult(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"user-error"}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted, terminal decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	if err := dec.Decode(&terminal); err != nil || terminal.Type != "error" || terminal.Code != "user_error" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
	if string(terminal.UserError) != `{"message":"boom"}` || string(terminal.Result) != `{"console":["before throw"]}` {
		t.Fatalf("user error = %s, result = %s", terminal.UserError, terminal.Result)
	}
}

type terminalFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes    int
	onTerminal func()
}

func (r *terminalFlushRecorder) Flush() {
	r.flushes++
	if r.flushes == 2 {
		r.onTerminal()
	}
	r.ResponseRecorder.Flush()
}

func TestTerminalFlushHoldsLane(t *testing.T) {
	h := testDaemon(t, Config{MaxConcurrency: 1}).Handler()
	recorder := &terminalFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		onTerminal: func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, taskRequest(t, `{"taskId":"next"}`, nil))
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("status during terminal flush = %d, want 429", w.Code)
			}
		},
	}
	h.ServeHTTP(recorder, taskRequest(t, `{"taskId":"first"}`, nil))
	if recorder.flushes != 2 {
		t.Fatalf("flushes = %d, want 2", recorder.flushes)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, taskRequest(t, `{"taskId":"next"}`, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status after terminal flush = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestCancelledBeforeDispatchReleasesTaskID(t *testing.T) {
	d := testDaemon(t, Config{})
	if _, err := d.getWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := d.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, taskRequest(t, `{"taskId":"retry"}`, nil).WithContext(ctx))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status before dispatch = %d, want 503: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, taskRequest(t, `{"taskId":"retry"}`, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", w.Code, w.Body.String())
	}
}
