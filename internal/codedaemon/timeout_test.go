package codedaemon

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTaskTimeoutCanOnlyShortenServiceLimit(t *testing.T) {
	t.Setenv("CODE_DAEMON_DEFAULT_TASK_TIMEOUT", "2s")
	t.Setenv("CODE_DAEMON_MAX_TASK_TIMEOUT", "3s")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := New(cfg)
	for request, want := range map[int64]time.Duration{0: 2 * time.Second, 250: 250 * time.Millisecond, 2500: 2 * time.Second, math.MaxInt64: 2 * time.Second} {
		if got := d.taskTimeout(request); got != want {
			t.Errorf("timeout for %d = %s, want %s", request, got, want)
		}
	}
	t.Setenv("CODE_DAEMON_DEFAULT_TASK_TIMEOUT", "4s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("accepted a default longer than the maximum")
	}
}

func TestHungTaskTimesOut(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"hang","timeoutMs":50}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted, terminal decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	if err := dec.Decode(&terminal); err != nil || terminal.Type != "error" || terminal.Code != "timeout" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
}
