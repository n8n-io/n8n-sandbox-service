package docker

import (
	"slices"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func TestCodeContainerArgs(t *testing.T) {
	opts := &runnerruntime.CreateOptions{Egress: runnerruntime.EgressNone, Kind: runnerruntime.KindCode, Runtime: "n8n-js", Slots: 2, TaskMemoryMB: 512, SpareWorkers: true, ReuseWorkers: true}
	if got := codeMemoryLimitMB(opts, 256); got != 1280 {
		t.Fatalf("memory limit %d", got)
	}
	args := dockerContainerCreateArgs("id", "name", "img", runnerNoEgressNetwork, &ResourceLimits{MemoryMB: 1280}, true, codeDaemonEnv(opts)...)
	idx := slices.Index(args, "--restart")
	if idx < 0 || args[idx+1] != "no" {
		t.Fatalf("restart policy: %v", args)
	}
	for _, want := range []string{"--user", "1000:1000", "--cap-drop", "ALL", "no-new-privileges", "CODE_DAEMON_SLOTS=2", "CODE_DAEMON_REUSE_WORKERS=true", "--memory", "1280m"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	general := dockerContainerCreateArgs("id", "name", "img", runnerBridgeNetwork, nil, true)
	if i := slices.Index(general, "--restart"); general[i+1] != "unless-stopped" {
		t.Fatalf("general restart policy changed: %v", general)
	}
}

func TestCodeOptionsValidation(t *testing.T) {
	for _, js := range []string{
		`{"egress":"public","kind":"code","runtime":"n8n-js","slots":1,"taskMemoryMb":512}`,
		`{"egress":"none","kind":"code","runtime":"n8n-js","slots":32,"taskMemoryMb":16384}`,
		`{"egress":"none","slots":2}`,
		`{"egress":"none","kind":"other"}`,
	} {
		if _, err := runnerruntime.ParseCreateOptions(js); err == nil {
			t.Errorf("accepted %s", js)
		}
	}
	if _, err := runnerruntime.ParseCreateOptions(`{"egress":"none","kind":"code","runtime":"n8n-js","slots":4,"taskMemoryMb":512,"spareWorkers":true}`); err != nil {
		t.Fatal(err)
	}
}
