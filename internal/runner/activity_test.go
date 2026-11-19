package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/activity"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const activitySandboxID = "11111111-1111-4111-8111-111111111111"

// An execution counts as activity while its stream is open and after the
// stream ends early, until the poller sees its exit; one seen to exit stops
// counting at once.
func TestExecProxyTracksActivity(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	exited := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(startedEvent("e1") + "\n" + stdoutEvent(1, "hi") + "\n"))
			w.(http.Flusher).Flush()
			<-release // the client goes away mid-run
		case r.Method == http.MethodGet:
			if got := r.URL.Query().Get("after"); got != "1" {
				t.Errorf("poll after = %q, want 1: only events after the last seen", got)
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			mu.Lock()
			done := exited
			mu.Unlock()
			if done {
				_, _ = w.Write([]byte(exitEvent(2) + "\n"))
			}
		}
	}))
	defer daemon.Close()
	defer close(release)

	tr := activity.New()
	rt := &fakeRuntime{daemonURL: daemon.URL}
	handler := ExecProxyHandler(rt, &config.Config{}, metrics.NewRunnerRecorder(false), tr)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+activitySandboxID+"/executions",
		strings.NewReader(`{"command":"sleep 3600","exec_id":"e1","timeout_ms":3600000}`)).WithContext(ctx)
	req.SetPathValue("id", activitySandboxID)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	waitFor(t, func() bool { return tr.Snapshot().Streams == 1 })
	if !tr.Active(activitySandboxID) {
		t.Fatal("sandbox with an open stream not active")
	}
	cancel()
	<-done
	if s := tr.Snapshot(); s.Streams != 0 || s.Executions != 1 {
		t.Fatalf("after disconnect: %+v, want the execution still counted", s)
	}

	client := &http.Client{}
	pollUnwatchedOnce(context.Background(), rt, tr, client)
	if !tr.Active(activitySandboxID) {
		t.Fatal("poll ended a running execution")
	}
	mu.Lock()
	exited = true
	mu.Unlock()
	pollUnwatchedOnce(context.Background(), rt, tr, client)
	if tr.Active(activitySandboxID) {
		t.Fatal("exited execution still active")
	}
}

func TestExecProxyEndsActivityOnExit(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(startedEvent("e1") + "\n" + exitEvent(1) + "\n"))
	}))
	defer daemon.Close()
	tr := activity.New()
	handler := ExecProxyHandler(&fakeRuntime{daemonURL: daemon.URL}, &config.Config{}, metrics.NewRunnerRecorder(false), tr)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+activitySandboxID+"/executions", strings.NewReader(`{"command":"true"}`))
	req.SetPathValue("id", activitySandboxID)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if tr.Active(activitySandboxID) {
		t.Fatalf("finished execution still active: %+v", tr.Snapshot())
	}
}

// The poller never wakes a stopped sandbox: it has no executions left.
func TestPollForgetsStoppedSandboxes(t *testing.T) {
	tr := activity.New()
	tr.ExecStarted(activitySandboxID, "e1", time.Hour)
	rt := &fakeRuntime{daemonErr: runnerruntime.ErrSandboxNotRunning}
	pollUnwatchedOnce(context.Background(), rt, tr, &http.Client{})
	if tr.Active(activitySandboxID) || rt.ensureCalls.Load() != 0 {
		t.Fatal("stopped sandbox kept active or woken")
	}
}

func TestPollTreatsMissingExecutionAsEnded(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer daemon.Close()
	tr := activity.New()
	tr.ExecStarted(activitySandboxID, "e1", time.Hour)
	pollUnwatchedOnce(context.Background(), &fakeRuntime{daemonURL: daemon.URL}, tr, &http.Client{})
	if tr.Active(activitySandboxID) {
		t.Fatal("execution the daemon no longer has is still active")
	}
}

// A 410 (history trimmed past what was seen) is undecided: the deadline ends it.
func TestPollKeepsExecutionOnGone(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer daemon.Close()
	tr := activity.New()
	tr.ExecStarted(activitySandboxID, "e1", time.Hour)
	pollUnwatchedOnce(context.Background(), &fakeRuntime{daemonURL: daemon.URL}, tr, &http.Client{})
	if !tr.Active(activitySandboxID) {
		t.Fatal("410 ended the execution")
	}
}

// A follow stream on the GET route counts while it is open; a one-shot read
// does not.
func TestFollowStreamCountsAsActivity(t *testing.T) {
	tr := activity.New()
	var during bool
	h := countFollowStream(tr, func(http.ResponseWriter, *http.Request) { during = tr.Active(activitySandboxID) })
	for _, tc := range []struct {
		query string
		want  bool
	}{{"follow=true", true}, {"", false}} {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+activitySandboxID+"/executions/e1?"+tc.query, nil)
		req.SetPathValue("id", activitySandboxID)
		h(httptest.NewRecorder(), req)
		if during != tc.want || tr.Active(activitySandboxID) {
			t.Fatalf("%q: active during = %v, after = %v", tc.query, during, tr.Active(activitySandboxID))
		}
	}
}

// The runner refuses to stop a sandbox with a run in progress, whatever the
// sweeper believes, and a stop or delete ends the sandbox's executions.
func TestStopRefusedWhileActive(t *testing.T) {
	tr := activity.New()
	rt := &stopWakeRuntime{}
	srv := &SandboxControlGRPC{
		Runtime:  rt,
		Cfg:      &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:      metrics.NewRunnerRecorder(false),
		Activity: tr,
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	closeStream := tr.StreamOpened(activitySandboxID)
	_, err := srv.StopSandbox(ctx, &pb.StopSandboxRequest{SandboxId: activitySandboxID})
	if status.Code(err) != codes.FailedPrecondition || rt.stopCalls != 0 {
		t.Fatalf("stop under a run: err=%v stopCalls=%d", err, rt.stopCalls)
	}
	closeStream()
	tr.ExecStarted(activitySandboxID, "e1", time.Hour)
	if _, err := srv.StopSandbox(ctx, &pb.StopSandboxRequest{SandboxId: activitySandboxID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stop with an unfinished execution: %v", err)
	}
	tr.ExecEnded(activitySandboxID, "e1")
	if _, err := srv.StopSandbox(ctx, &pb.StopSandboxRequest{SandboxId: activitySandboxID}); err != nil || rt.stopCalls != 1 {
		t.Fatalf("idle stop: err=%v stopCalls=%d", err, rt.stopCalls)
	}
}

func TestGetActivityReportsDrainState(t *testing.T) {
	tr := activity.New()
	cd := cordon.New()
	srv := &SandboxControlGRPC{
		Runtime:  &fakeRuntime{},
		Cfg:      &config.Config{APIKeys: map[string]struct{}{"runner-key": {}}},
		Rec:      metrics.NewRunnerRecorder(false),
		Cordon:   cd,
		Activity: tr,
	}
	tr.ExecStarted(activitySandboxID, "e1", time.Hour)
	defer tr.StreamOpened(activitySandboxID)()
	cd.Set(true)
	if _, err := srv.GetActivity(context.Background(), &pb.GetActivityRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "runner-key"))
	resp, err := srv.GetActivity(ctx, &pb.GetActivityRequest{})
	if err != nil || !resp.GetCordoned() || resp.GetActiveExecutions() != 1 || resp.GetOpenStreams() != 1 {
		t.Fatalf("GetActivity = %+v, %v", resp, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A 5 h execution is accepted under an 8 h cap, and counts as activity until
// its 5 h timeout, not the default one.
func TestExecProxyAcceptsMultiHourTimeout(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(startedEvent("e1") + "\n"))
	}))
	defer daemon.Close()
	tr := activity.New()
	handler := ExecProxyHandler(&fakeRuntime{daemonURL: daemon.URL}, &config.Config{MaxExecTimeout: 8 * time.Hour}, metrics.NewRunnerRecorder(false), tr)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+activitySandboxID+"/executions",
		strings.NewReader(`{"command":"run-cli","exec_id":"e1","timeout_ms":18000000}`))
	req.SetPathValue("id", activitySandboxID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("5 h exec: %d %s", rr.Code, rr.Body.String())
	}
	if !tr.Active(activitySandboxID) {
		t.Fatal("5 h execution not active after its stream ended")
	}
}
