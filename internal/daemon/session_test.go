package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestSessionFirstEventIsSession(t *testing.T) {
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
	if first.Type != ResponseTypeSession {
		t.Fatalf("expected first event type session, got %s", first.Type)
	}
	if first.ExecID == "" {
		t.Fatal("session event missing exec_id")
	}
	if first.Seq == nil || *first.Seq != 0 {
		t.Fatalf("expected seq 0 on session event, got %v", first.Seq)
	}
}

func TestSessionAllEventsHaveSeq(t *testing.T) {
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
		t.Fatalf("expected at least 3 events (session+stdout+exit), got %d", prevSeq+1)
	}
}

func TestSessionSameExecIDReturnsSameSession(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	sess1 := sm.GetOrCreate("exec-1", "echo hello", nil, "", 5*time.Second)
	sess2 := sm.GetOrCreate("exec-1", "echo world", nil, "", 5*time.Second)

	if sess1.ID != sess2.ID {
		t.Fatalf("expected same session ID, got %s and %s", sess1.ID, sess2.ID)
	}
}

func TestSessionEmptyExecIDGeneratesUnique(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	sess1 := sm.GetOrCreate("", "echo hello", nil, "", 5*time.Second)
	sess2 := sm.GetOrCreate("", "echo hello", nil, "", 5*time.Second)

	if sess1.ID == sess2.ID {
		t.Fatal("empty exec_id should generate separate sessions")
	}
}

func TestSessionCancel(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	sess := sm.GetOrCreate("", "sleep 30", nil, "", 30*time.Second)

	// Let the command start before cancelling
	time.Sleep(200 * time.Millisecond)

	if !sm.Cancel(sess.ID) {
		t.Fatal("expected Cancel to return true")
	}

	// Follow with a long context to wait for the exit event after kill
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess.Follow(ctx, nil, func([]byte) {})

	raw, ok := sess.Snapshot(nil)
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

func TestSessionCancelNotFound(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	if sm.Cancel("nonexistent") {
		t.Fatal("expected Cancel to return false for nonexistent session")
	}
}

func TestSessionResumeReturnsNoduplicates(t *testing.T) {
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

	// Parse all events from POST to get the exec_id and last seq
	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	var lastSeq uint64
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeSession {
			execID = resp.ExecID
		}
		if resp.Seq != nil {
			lastSeq = *resp.Seq
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id from session event")
	}

	// Resume from after the last seq we got
	resumeURL := "/exec/" + execID + "?after=" + strings.Replace(
		strings.TrimRight(strings.TrimRight(
			strings.Replace(
				string(rune(lastSeq+'0')), "", "", 0,
			), ""), ""), "", "", 0,
	)
	_ = resumeURL // unused, using formatted URL below

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

	// Resume should return no events since we already got everything
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

func TestSessionResumePartial(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	sess := sm.GetOrCreate("", "echo hello", nil, "", 5*time.Second)

	// Wait for the session to complete
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess.Follow(ctx, nil, func([]byte) {})

	// Get all events
	allRaw, ok := sess.Snapshot(nil)
	if !ok {
		t.Fatal("snapshot failed")
	}
	if len(allRaw) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(allRaw))
	}

	// Resume from after seq 1 (skip session + stdout)
	partialRaw, ok := sess.Snapshot(afterSeq(1))
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

func TestSessionNonexistentReturns404(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	body := `{"command":"echo hello"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Parse exec_id
	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeSession {
			execID = resp.ExecID
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id")
	}

	// Requesting a nonexistent session returns 404
	resumeReq := httptest.NewRequest(http.MethodGet, "/exec/nonexistent?after=0", nil)
	resumeRR := httptest.NewRecorder()
	handler.ServeHTTP(resumeRR, resumeReq)
	if resumeRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent session, got %d", resumeRR.Code)
	}
}

func TestDeleteExecEndpoint(t *testing.T) {
	handler := NewHandler(t.TempDir())
	t.Cleanup(handler.Close)

	// Start a long-running command
	body := `{"command":"sleep 30"}`
	req := httptest.NewRequest(http.MethodPost, "/exec", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	// Use a cancellable context so the POST handler returns quickly
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Parse exec_id from the session event
	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	var execID string
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Type == ResponseTypeSession {
			execID = resp.ExecID
		}
	}
	if execID == "" {
		t.Fatal("missing exec_id from session event")
	}

	// DELETE the session
	delReq := httptest.NewRequest(http.MethodDelete, "/exec/"+execID, nil)
	delRR := httptest.NewRecorder()
	handler.ServeHTTP(delRR, delReq)

	if delRR.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", delRR.Code)
	}

	// DELETE nonexistent session returns 404
	delReq2 := httptest.NewRequest(http.MethodDelete, "/exec/nonexistent", nil)
	delRR2 := httptest.NewRecorder()
	handler.ServeHTTP(delRR2, delReq2)
	if delRR2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent session, got %d", delRR2.Code)
	}
}

func TestDisconnectDoesNotKillCommand(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Close()

	sess := sm.GetOrCreate("", "echo alive", nil, "", 5*time.Second)

	// Follow with a short-lived context (simulates client disconnect)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sess.Follow(ctx, nil, func([]byte) {})

	// The command should still complete even after we disconnected.
	// Wait and check that the exit event was eventually produced.
	time.Sleep(500 * time.Millisecond)

	raw, ok := sess.Snapshot(nil)
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
