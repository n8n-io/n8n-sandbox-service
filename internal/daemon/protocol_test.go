package daemon

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecResponseMarshalStartsWithSeqAndType(t *testing.T) {
	seq := uint64(7)
	tests := []Response{
		{Seq: &seq, Type: ResponseTypeStarted, ExecID: "exec-1"},
		{Seq: &seq, Type: ResponseTypeStdout, Data: "hello\n"},
		{Seq: &seq, Type: ResponseTypeStderr, Data: "warn\n"},
		{Seq: &seq, Type: ResponseTypeExit, ExitCode: 0},
		{Seq: &seq, Type: ResponseTypeError, Error: "failed"},
	}

	for _, resp := range tests {
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal %s response: %v", resp.Type, err)
		}

		wantPrefix := []byte(`{"seq":7,"type":"` + string(resp.Type) + `"`)
		if !bytes.HasPrefix(data, wantPrefix) {
			t.Fatalf("expected %s response to start with %s, got %s", resp.Type, wantPrefix, data)
		}
	}
}

func TestResponseMarshalOmitsExecFlagsForNonExit(t *testing.T) {
	resp := Response{
		Type: ResponseTypeStdout,
		Data: "hello\n",
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"success"`) || strings.Contains(s, `"timed_out"`) || strings.Contains(s, `"killed"`) {
		t.Fatalf("non-exit response should not include exec flags: %s", s)
	}
}

func TestResponseMarshalIncludesFalseExecFlagsOnExit(t *testing.T) {
	success := false
	timedOut := false
	killed := false
	resp := Response{
		Type:     ResponseTypeExit,
		ExitCode: 1,
		Success:  &success,
		TimedOut: &timedOut,
		Killed:   &killed,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	s := string(b)
	for _, key := range []string{`"success":false`, `"timed_out":false`, `"killed":false`} {
		if !strings.Contains(s, key) {
			t.Fatalf("exit response missing %s in %s", key, s)
		}
	}
}
