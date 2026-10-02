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

// The runner relays daemon bodies, so a body alone is not the runner speaking.
func TestRunnerReportsSandboxGoneIgnoresBodyWithoutHeader(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"` + runnerruntime.ErrSandboxNotFound.Error() + `"}`)),
	}
	if RunnerReportsSandboxGone(resp) {
		t.Fatal("a sandbox-not-found body without the header must not trigger sandbox reap")
	}
}

func TestStripSignals(t *testing.T) {
	h := http.Header{"Content-Type": []string{"application/json"}}
	MarkSandboxGone(h)
	MarkSandboxRestarted(h)
	h.Add("x-sandbox-gone", "1")

	StripSignals(h)

	if len(h) != 1 || h.Get("Content-Type") != "application/json" {
		t.Fatalf("header = %v, want only Content-Type left", h)
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
