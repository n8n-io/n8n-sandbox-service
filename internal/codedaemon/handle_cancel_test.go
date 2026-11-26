package codedaemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func beginTask(t *testing.T, client *http.Client, url, id string) *json.Decoder {
	t.Helper()
	req := taskRequest(t, `{"taskId":"`+id+`","timeoutMs":60000}`, nil)
	res, err := client.Post(url+"/tasks", req.Header.Get("Content-Type"), req.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	dec := json.NewDecoder(res.Body)
	var event decodedEvent
	if err := dec.Decode(&event); err != nil || event.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", event, err)
	}
	return dec
}

func TestCancelRunningTask(t *testing.T) {
	for _, id := range []string{"cooperative", "hang"} {
		t.Run(id, func(t *testing.T) {
			d := testDaemon(t, Config{MaxConcurrency: 3})
			srv := httptest.NewServer(d.Handler())
			t.Cleanup(srv.Close)
			client := &http.Client{Timeout: cancelGrace + 5*time.Second}
			var sibling *json.Decoder
			if id == "hang" {
				sibling = beginTask(t, client, srv.URL, "pending")
			}
			dec := beginTask(t, client, srv.URL, id)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, srv.URL+"/tasks/"+id, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusAccepted {
				t.Fatalf("delete status = %d", res.StatusCode)
			}
			health, err := client.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = health.Body.Close()
			if health.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("healthz status = %d after the runtime retired, want 503", health.StatusCode)
			}
			late := taskRequest(t, `{"taskId":"late","timeoutMs":60000}`, nil)
			lateRes, err := client.Post(srv.URL+"/tasks", late.Header.Get("Content-Type"), late.Body)
			if err != nil {
				t.Fatal(err)
			}
			var lateEvent decodedEvent
			err = json.NewDecoder(lateRes.Body).Decode(&lateEvent)
			_ = lateRes.Body.Close()
			if err != nil || lateEvent.Type != "error" || lateEvent.Code != "crashed" {
				t.Fatalf("task started on a retired runtime: %+v, %v", lateEvent, err)
			}
			var event decodedEvent
			if err := decodeSkippingHeartbeats(dec, &event); err != nil || event.Type != "error" || event.Code != "cancelled" {
				t.Fatalf("cancellation event = %+v, %v", event, err)
			}
			if sibling != nil {
				if err := decodeSkippingHeartbeats(sibling, &event); err != nil || event.Type != "error" || event.Code != "crashed" {
					t.Fatalf("sibling event = %+v, %v", event, err)
				}
				if err := sibling.Decode(&event); !errors.Is(err, io.EOF) {
					t.Fatalf("sibling stream did not finish: %v", err)
				}
			}
			if err := dec.Decode(&event); !errors.Is(err, io.EOF) {
				t.Fatalf("cancelled stream did not finish: %v", err)
			}
			d.supervisor.workerMu.Lock()
			w := d.supervisor.worker
			d.supervisor.workerMu.Unlock()
			select {
			case <-w.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the retired runtime did not exit after its last task")
			}
		})
	}
}

func TestCancelUnknownTask(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/tasks/unknown", nil))
	var body taskRejection
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || w.Code != http.StatusNotFound || body.Code != "not_found" {
		t.Fatalf("status = %d, body = %+v, %v", w.Code, body, err)
	}
}
