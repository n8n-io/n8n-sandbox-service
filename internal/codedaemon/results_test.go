package codedaemon

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestUserErrorInlinesResult(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"user-error"}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted, terminal decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	if err := dec.Decode(&terminal); err != nil || terminal.Type != "error" || terminal.Code != "user_error" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
	if string(terminal.UserError) != `{"message":"boom"}` || string(terminal.Result) != `{"console":["before throw"]}` {
		t.Fatalf("user error = %s, result = %s", terminal.UserError, terminal.Result)
	}
}

func TestDoneWithoutOutputsHasEmptyManifest(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"no-outputs"}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	var terminal map[string]json.RawMessage
	if err := dec.Decode(&terminal); err != nil || string(terminal["type"]) != `"done"` {
		t.Fatalf("terminal = %v, %v", terminal, err)
	}
	if string(terminal["outputs"]) != `[]` || string(terminal["result"]) != `null` {
		t.Fatalf("outputs = %s, result = %s", terminal["outputs"], terminal["result"])
	}
}
