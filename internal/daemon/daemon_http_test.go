package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
