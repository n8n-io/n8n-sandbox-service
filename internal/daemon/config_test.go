package daemon

import (
	"testing"
	"time"
)

func TestLoadExecConfigDefaults(t *testing.T) {
	t.Setenv("SANDBOX_EXEC_MAX_EVENT_BYTES", "")
	t.Setenv("SANDBOX_EXEC_RETAIN", "")
	cfg, err := LoadExecConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg != DefaultExecConfig() {
		t.Fatalf("got %+v, want the defaults", cfg)
	}
}

func TestLoadExecConfigFromEnv(t *testing.T) {
	t.Setenv("SANDBOX_EXEC_MAX_EVENT_BYTES", " 1024 ")
	t.Setenv("SANDBOX_EXEC_RETAIN", "1h")
	cfg, err := LoadExecConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxEventBytes != 1024 || cfg.RetainPeriod != time.Hour {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadExecConfigRefusesBadValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"SANDBOX_EXEC_MAX_EVENT_BYTES", "lots"},
		{"SANDBOX_EXEC_MAX_EVENT_BYTES", "0"},
		{"SANDBOX_EXEC_RETAIN", "10"},
		{"SANDBOX_EXEC_RETAIN", "-1m"},
	} {
		t.Setenv("SANDBOX_EXEC_MAX_EVENT_BYTES", "")
		t.Setenv("SANDBOX_EXEC_RETAIN", "")
		t.Setenv(tc.name, tc.value)
		if _, err := LoadExecConfig(); err == nil {
			t.Fatalf("%s=%q: no error", tc.name, tc.value)
		}
	}
}
