package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryMiddlewareRethrowsAbortHandler(t *testing.T) {
	handler := RecoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	recovered := recoverPanic(func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/exec", nil))
	})

	if recovered != http.ErrAbortHandler {
		t.Fatalf("expected http.ErrAbortHandler panic, got %v", recovered)
	}
}

func TestRecoveryMiddlewareWritesErrorBeforeResponseStarts(t *testing.T) {
	handler := RecoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/exec", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "internal server error") {
		t.Fatalf("expected internal server error body, got %q", rr.Body.String())
	}
}

func TestRecoveryMiddlewareAbortsAfterResponseStarts(t *testing.T) {
	handler := RecoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"exit\"}\n"))
		panic("boom")
	}))

	rr := httptest.NewRecorder()
	recovered := recoverPanic(func() {
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/exec", nil))
	})

	if recovered != http.ErrAbortHandler {
		t.Fatalf("expected http.ErrAbortHandler panic, got %v", recovered)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	if strings.Contains(rr.Body.String(), "internal server error") {
		t.Fatalf("unexpected error appended to response body: %q", rr.Body.String())
	}
}

func recoverPanic(fn func()) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	fn()
	return nil
}
