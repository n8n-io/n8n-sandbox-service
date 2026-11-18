package codedaemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadRuntime(t *testing.T) {
	tests := []struct {
		name     string
		registry string // empty means no registry file
		runtime  string
		want     []string
	}{
		{name: "default runtime", registry: `{"n8n-js":{"command":["node","main.js"]},"other":{"command":["x"]}}`, want: []string{"node", "main.js"}},
		{name: "selected runtime", registry: `{"n8n-js":{"command":["node"]},"other":{"command":["x","-y"]}}`, runtime: "other", want: []string{"x", "-y"}},
		{name: "missing file"},
		{name: "invalid JSON", registry: `{"n8n-js":`},
		{name: "unknown runtime", registry: `{"n8n-js":{"command":["node"]}}`, runtime: "python"},
		{name: "empty command", registry: `{"n8n-js":{"command":[]}}`},
		{name: "empty executable", registry: `{"n8n-js":{"command":["","main.js"]}}`},
		{name: "missing command", registry: `{"n8n-js":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "runtimes.json")
			if tt.registry != "" {
				if err := os.WriteFile(registry, []byte(tt.registry), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			workDir := filepath.Join(dir, "work")
			t.Setenv("CODE_DAEMON_WORK_DIR", workDir)
			t.Setenv("CODE_DAEMON_RUNTIMES_FILE", registry)
			t.Setenv("CODE_DAEMON_RUNTIME", tt.runtime)

			var cfg Config
			err := cfg.LoadRuntime()
			if tt.want == nil {
				if err == nil {
					t.Fatalf("got command %q, want an error", cfg.RuntimeCommand)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.RuntimeCommand, tt.want) || cfg.WorkDir != workDir {
				t.Fatalf("got command %q in %q, want %q in %q", cfg.RuntimeCommand, cfg.WorkDir, tt.want, workDir)
			}
			if info, err := os.Stat(workDir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("work dir not created with mode 0700: %v %v", info, err)
			}
		})
	}
}
