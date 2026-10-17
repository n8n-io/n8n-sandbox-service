package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// forgettingRuntime records the daemon URLs a proxy forgets.
type forgettingRuntime struct {
	*fakeRuntime
	mu        sync.Mutex
	forgotten []string
}

func (f *forgettingRuntime) ForgetDaemonURL(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, sandboxID)
}

// A runtime that caches daemon URLs can hand out the address of a container
// that died before it heard of the death. The proxy is the first to find out,
// and it must drop the URL so that the next request looks the sandbox up again
// instead of failing the same way.
func TestProxiesForgetADaemonURLThatDoesNotAnswer(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cases := map[string]struct {
		handler func(rt *forgettingRuntime) http.HandlerFunc
		request func() *http.Request
	}{
		"proxy": {
			handler: func(rt *forgettingRuntime) http.HandlerFunc {
				return ProxyHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false))
			},
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+proxyTestSandboxID+"/files", nil)
				req.SetPathValue("id", proxyTestSandboxID)
				return req
			},
		},
		"exec proxy": {
			handler: func(rt *forgettingRuntime) http.HandlerFunc {
				return ExecProxyHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false))
			},
			request: proxyTestRequest,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rt := &forgettingRuntime{fakeRuntime: &fakeRuntime{daemonURL: deadURL}}
			rec := httptest.NewRecorder()
			tc.handler(rt).ServeHTTP(rec, tc.request())

			if rec.Code < http.StatusInternalServerError {
				t.Fatalf("status = %d, want a 5xx for a daemon that does not answer", rec.Code)
			}
			if len(rt.forgotten) != 1 || rt.forgotten[0] != proxyTestSandboxID {
				t.Errorf("forgotten = %v, want [%s]", rt.forgotten, proxyTestSandboxID)
			}
		})
	}
}

// A client that gave up on a daemon that does not answer must not leave its
// URL in the cache for the next request.
func TestProxyForgetsTheDaemonURLWhenTheClientHasGone(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	rt := &forgettingRuntime{fakeRuntime: &fakeRuntime{daemonURL: deadURL}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+proxyTestSandboxID+"/files", nil).WithContext(ctx)
	req.SetPathValue("id", proxyTestSandboxID)

	ProxyHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(httptest.NewRecorder(), req)

	if len(rt.forgotten) != 1 || rt.forgotten[0] != proxyTestSandboxID {
		t.Errorf("forgotten = %v, want [%s]", rt.forgotten, proxyTestSandboxID)
	}
}
