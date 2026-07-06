package sandboxproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func TestRunnerReportsSandboxGoneHeader(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{SandboxGoneHeader: []string{"1"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"sandbox not found"}`)),
	}
	if !RunnerReportsSandboxGone(resp) {
		t.Fatal("expected sandbox gone with header")
	}
}

func TestRunnerReportsSandboxGoneBodyFallback(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"` + runnerruntime.ErrSandboxNotFound.Error() + `"}`)),
	}
	if !RunnerReportsSandboxGone(resp) {
		t.Fatal("expected sandbox gone from body fallback")
	}
}

func TestRunnerReportsSandboxGoneRejectsExecutionNotFound(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"execution not found"}`)),
	}
	if RunnerReportsSandboxGone(resp) {
		t.Fatal("execution not found must not trigger sandbox reap")
	}
}

func TestRunnerReportsSandboxGonePreservesBody(t *testing.T) {
	body := `{"error":"sandbox not found"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{SandboxGoneHeader: []string{"1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if !RunnerReportsSandboxGone(resp) {
		t.Fatal("expected sandbox gone")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() failed: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want preserved for client", string(got))
	}
}
