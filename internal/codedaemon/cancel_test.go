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
	req := taskRequest(t, `{"taskId":"`+id+`"}`, nil)
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
			d := testDaemon(t, Config{MaxConcurrency: 2})
			srv := httptest.NewServer(d.Handler())
			t.Cleanup(srv.Close)
			client := &http.Client{Timeout: 5 * time.Second}
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
			var event decodedEvent
			if err := dec.Decode(&event); err != nil || event.Type != "error" || event.Code != "cancelled" {
				t.Fatalf("cancellation event = %+v, %v", event, err)
			}
			if sibling != nil {
				if err := sibling.Decode(&event); err != nil || event.Type != "error" || event.Code != "crashed" {
					t.Fatalf("sibling event = %+v, %v", event, err)
				}
				if err := sibling.Decode(&event); !errors.Is(err, io.EOF) {
					t.Fatalf("sibling stream did not finish: %v", err)
				}
			}
			if err := dec.Decode(&event); !errors.Is(err, io.EOF) {
				t.Fatalf("cancelled stream did not finish: %v", err)
			}
			next := beginTask(t, client, srv.URL, "after-cancel")
			if err := next.Decode(&event); err != nil || event.Type != "done" {
				t.Fatalf("follow-up event = %+v, %v", event, err)
			}
		})
	}
}
