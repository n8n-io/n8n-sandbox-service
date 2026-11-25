package runtime

import (
	"encoding/json"
	"testing"
)

func TestParseCreateOptionsCode(t *testing.T) {
	opts, err := ParseCreateOptions(`{"egress":"none","code_options":{"memory_mb":1024}}`)
	if err != nil {
		t.Fatalf("ParseCreateOptions() error = %v", err)
	}
	if opts.CodeOpts == nil || opts.CodeOpts.MemoryMB != 1024 {
		t.Fatalf("code = %+v, want memory_mb 1024", opts.CodeOpts)
	}

	opts, err = ParseCreateOptions(`{"egress":"none"}`)
	if err != nil || opts.CodeOpts != nil {
		t.Fatalf("ParseCreateOptions(no code) = %+v, %v; want no code", opts, err)
	}
}

func TestParseCreateOptionsRefusesInvalidCode(t *testing.T) {
	for name, raw := range map[string]string{
		"no memory":       `{"code_options":{}}`,
		"zero memory":     `{"code_options":{"memory_mb":0}}`,
		"negative memory": `{"code_options":{"memory_mb":-1}}`,
		"under minimum":   `{"egress":"none","code_options":{"memory_mb":63}}`,
		"public egress":   `{"egress":"public","code_options":{"memory_mb":512}}`,
		"default egress":  `{"code_options":{"memory_mb":512}}`,
		"volumes":         `{"egress":"none","code_options":{"memory_mb":512},"volumes":[` + testVolumeJSON + `]}`,
	} {
		if _, err := ParseCreateOptions(raw); err == nil {
			t.Errorf("%s: ParseCreateOptions(%s) succeeded", name, raw)
		}
	}
}

func TestAppliedCreateOptionsEchoesCode(t *testing.T) {
	opts := &CreateOptions{Egress: EgressNone, CodeOpts: &CodeOptions{MemoryMB: 256}}
	raw, err := json.Marshal(opts.Applied(nil))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := ParseCreateOptions(string(raw))
	if err != nil {
		t.Fatalf("ParseCreateOptions(applied) error = %v", err)
	}
	if applied.CodeOpts == nil || *applied.CodeOpts != *opts.CodeOpts {
		t.Fatalf("applied code = %+v, want %+v", applied.CodeOpts, opts.CodeOpts)
	}
}
