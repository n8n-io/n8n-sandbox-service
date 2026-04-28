package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

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
