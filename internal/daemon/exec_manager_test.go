package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func parseEvents(raw [][]byte) []Response {
	out := make([]Response, len(raw))
	for i, data := range raw {
		if err := json.Unmarshal(data, &out[i]); err != nil {
			panic("parseEvents: " + err.Error())
		}
	}
	return out
}

func afterSeq(n uint64) *uint64 { return &n }

func TestExecutionFirstEventIsStarted(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"echo hello"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var first Response
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode first event: %v", err)
	}
	if first.Type != ResponseTypeStarted {
		t.Fatalf("expected first event type started, got %s", first.Type)
	}
	if first.ExecID == "" {
		t.Fatal("started event missing exec_id")
	}
	if first.Seq == nil || *first.Seq != 0 {
		t.Fatalf("expected seq 0 on started event, got %v", first.Seq)
	}
}

func TestExecutionAllEventsHaveSeq(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"echo hello"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var prevSeq int64 = -1
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Seq == nil {
			t.Fatalf("event type %s missing seq", resp.Type)
		}
		seq := int64(*resp.Seq)
		if seq != prevSeq+1 {
			t.Fatalf("expected seq %d, got %d (type=%s)", prevSeq+1, seq, resp.Type)
		}
		prevSeq = seq
	}
	if prevSeq < 2 {
		t.Fatalf("expected at least 3 events (started+stdout+exit), got %d", prevSeq+1)
	}
}

func TestExecutionSameExecIDReturnsSameExecution(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	ex1 := em.GetOrCreate("exec-1", "echo hello", nil, "", 5*time.Second)
	ex2 := em.GetOrCreate("exec-1", "echo world", nil, "", 5*time.Second)

	if ex1.ID != ex2.ID {
		t.Fatalf("expected same execution ID, got %s and %s", ex1.ID, ex2.ID)
	}
}

func TestExecutionEmptyExecIDGeneratesUnique(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	ex1 := em.GetOrCreate("", "echo hello", nil, "", 5*time.Second)
	ex2 := em.GetOrCreate("", "echo hello", nil, "", 5*time.Second)

	if ex1.ID == ex2.ID {
		t.Fatal("empty exec_id should generate separate executions")
	}
}

func TestExecutionCancel(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	ex := em.GetOrCreate("", "sleep 30", nil, "", 30*time.Second)

	time.Sleep(200 * time.Millisecond)

	if !em.Cancel(ex.ID) {
		t.Fatal("expected Cancel to return true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ex.Follow(ctx, nil, func([]byte) {})

	raw, ok := ex.Snapshot(nil)
	if !ok {
		t.Fatal("expected snapshot to succeed")
	}
	events := parseEvents(raw)
	var sawExit bool
	for _, e := range events {
		if e.Type == ResponseTypeExit {
			sawExit = true
			if e.Killed == nil || !*e.Killed {
				t.Fatal("expected killed=true after cancel")
			}
		}
	}
	if !sawExit {
		t.Fatal("expected exit event after cancel")
	}
}

func TestExecutionCancelNotFound(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	if em.Cancel("nonexistent") {
		t.Fatal("expected Cancel to return false for nonexistent execution")
	}
}

func TestExecutionResumeReturnsNoDuplicates(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"echo hello","exec_id":"test-exec-id"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("POST expected 200, got %d", rr.Code)
	}

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	var lastSeq uint64
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeStarted {
			execID = resp.ExecID
		}
		if resp.Seq != nil {
			lastSeq = *resp.Seq
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id from started event")
	}

	resumeReq := httptest.NewRequest(http.MethodGet,
		"/exec/"+execID+"?after="+func() string {
			b, _ := json.Marshal(lastSeq)
			return string(b)
		}(),
		nil,
	)
	resumeRR := httptest.NewRecorder()
	handler.ServeHTTP(resumeRR, resumeReq)

	if resumeRR.Code != http.StatusOK {
		t.Fatalf("GET resume expected 200, got %d: %s", resumeRR.Code, resumeRR.Body.String())
	}

	resumeDec := json.NewDecoder(bytes.NewReader(resumeRR.Body.Bytes()))
	var resumeEvents int
	for {
		var resp Response
		if err := resumeDec.Decode(&resp); err != nil {
			break
		}
		resumeEvents++
	}
	if resumeEvents != 0 {
		t.Fatalf("expected 0 events on resume after last seq, got %d", resumeEvents)
	}
}

func TestExecutionResumePartial(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	ex := em.GetOrCreate("", "echo hello", nil, "", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ex.Follow(ctx, nil, func([]byte) {})

	allRaw, ok := ex.Snapshot(nil)
	if !ok {
		t.Fatal("snapshot failed")
	}
	if len(allRaw) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(allRaw))
	}

	partialRaw, ok := ex.Snapshot(afterSeq(1))
	if !ok {
		t.Fatal("partial snapshot failed")
	}

	partialEvents := parseEvents(partialRaw)
	for _, e := range partialEvents {
		if *e.Seq <= 1 {
			t.Fatalf("resume returned event with seq %d, expected > 1", *e.Seq)
		}
	}

	if len(partialRaw) != len(allRaw)-2 {
		t.Fatalf("expected %d events, got %d", len(allRaw)-2, len(partialRaw))
	}
}

func TestExecutionNonexistentReturns404(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"echo hello"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeStarted {
			execID = resp.ExecID
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id")
	}

	resumeReq := httptest.NewRequest(http.MethodGet, "/exec/nonexistent?after=0", nil)
	resumeRR := httptest.NewRecorder()
	handler.ServeHTTP(resumeRR, resumeReq)
	if resumeRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent execution, got %d", resumeRR.Code)
	}
}

func TestDeleteExecEndpoint(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"sleep 30"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeStarted {
			execID = resp.ExecID
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id from started event")
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/exec/"+execID, nil)
	delRR := httptest.NewRecorder()
	handler.ServeHTTP(delRR, delReq)

	if delRR.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", delRR.Code)
	}

	delReq2 := httptest.NewRequest(http.MethodDelete, "/exec/nonexistent", nil)
	delRR2 := httptest.NewRecorder()
	handler.ServeHTTP(delRR2, delReq2)
	if delRR2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent execution, got %d", delRR2.Code)
	}
}

func TestDisconnectDoesNotKillCommand(t *testing.T) {
	em := NewExecManager()
	defer em.Close()

	ex := em.GetOrCreate("", "echo alive", nil, "", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ex.Follow(ctx, nil, func([]byte) {})

	time.Sleep(500 * time.Millisecond)

	raw, ok := ex.Snapshot(nil)
	if !ok {
		t.Fatal("snapshot failed after disconnect")
	}

	events := parseEvents(raw)
	var sawExit bool
	for _, e := range events {
		if e.Type == ResponseTypeExit {
			sawExit = true
		}
	}
	if !sawExit {
		t.Fatal("expected exit event to be produced after client disconnect")
	}
}
