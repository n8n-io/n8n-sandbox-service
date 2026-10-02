package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRefuseRedirectReturnsTheRedirectUnfollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()

	for _, code := range []int{301, 302, 303, 307, 308} {
		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/executions", code)
		}))
		client := &http.Client{CheckRedirect: RefuseRedirect}
		resp, err := client.Post(daemon.URL+"/executions", "application/json", strings.NewReader(`{"command":"id"}`))
		daemon.Close()
		if err != nil {
			t.Fatalf("%d: Post() error = %v, want the redirect response itself", code, err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%d: status = %d, want the daemon's own", code, resp.StatusCode)
		}
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}
