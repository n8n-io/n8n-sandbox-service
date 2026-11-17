package codedaemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func finishTask(t *testing.T, dec *json.Decoder, code string) decodedEvent {
	t.Helper()
	var ev decodedEvent
	if err := dec.Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if code == "" && ev.Type != "done" || code != "" && (ev.Type != "error" || ev.Code != code) {
		t.Fatalf("terminal = %+v, want code %q", ev, code)
	}
	return ev
}

func resultPID(t *testing.T, ev decodedEvent) string {
	t.Helper()
	var values []string
	if err := json.Unmarshal(ev.Result, &values); err != nil || len(values) != 3 {
		t.Fatalf("result = %s, %v", ev.Result, err)
	}
	return values[0]
}

func TestSharedRuntimeReplacement(t *testing.T) {
	for _, tc := range []struct {
		id    string
		limit int
	}{
		{"count", 1},
		{"large_heap", 100},
	} {
		t.Run(tc.id, func(t *testing.T) {
			d := testDaemon(t, Config{RecycleAfterTasks: tc.limit, RecycleHeapBytes: 2048})
			srv := httptest.NewServer(d.Handler())
			t.Cleanup(srv.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			first := resultPID(t, finishTask(t, beginTask(t, client, srv.URL, tc.id), ""))
			next := resultPID(t, finishTask(t, beginTask(t, client, srv.URL, "next"), ""))
			if first == next {
				t.Fatal("daemon reused a retired runtime")
			}
		})
	}
}

func TestRuntimeExitFailsSiblingsAndRecovers(t *testing.T) {
	for _, code := range []string{"crashed", "oom"} {
		t.Run(code, func(t *testing.T) {
			d := testDaemon(t, Config{MaxConcurrency: 2})
			srv := httptest.NewServer(d.Handler())
			t.Cleanup(srv.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			initial := resultPID(t, finishTask(t, beginTask(t, client, srv.URL, "initial"), ""))
			pending := beginTask(t, client, srv.URL, "pending")
			id := "crash"
			if code == "oom" {
				id = "oom"
			}
			finishTask(t, beginTask(t, client, srv.URL, id), code)
			finishTask(t, pending, code)
			next := resultPID(t, finishTask(t, beginTask(t, client, srv.URL, "next"), ""))
			if initial == next {
				t.Fatal("daemon reused an exited runtime")
			}
		})
	}
}
