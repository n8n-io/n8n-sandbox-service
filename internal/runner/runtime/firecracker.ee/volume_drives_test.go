package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareJailLinksTheVolumeDrivePlaceholders(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)

	var prepareScript string
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		if script := args[len(args)-1]; strings.Contains(script, "mount --bind") && prepareScript == "" {
			prepareScript = script
		}
		return nil
	}

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	placeholder := "'" + filepath.Join(rt.config.JailerBaseDir, volumeDrivePlaceholderName) + "'"
	if want := "(umask 0333 && : > " + placeholder + ".$$ && mv -fT " + placeholder + ".$$ " + placeholder + ")"; !strings.Contains(prepareScript, want) {
		t.Errorf("prepare jail script does not create the shared placeholder read-only:\nwant %s\ngot\n%s", want, prepareScript)
	}
	vmDir := filepath.Join(rt.config.JailerBaseDir, "firecracker", "sandbox-"+shortID(sandboxID))
	for index := range 8 {
		staged := "'" + filepath.Join(vmDir, fmt.Sprintf(".vol%d.img", index)) + "'"
		target := "'" + filepath.Join(vmDir, "root", fmt.Sprintf("vol%d.img", index)) + "'"
		want := "ln -f " + placeholder + " " + staged + "\nmv -fT " + staged + " " + target + "\n"
		if !strings.Contains(prepareScript, want) {
			t.Errorf("prepare jail script does not link volume drive %d into place:\nwant %s\ngot\n%s", index, want, prepareScript)
		}
		if count := strings.Count(prepareScript, target); count != 1 {
			t.Errorf("prepare jail script names %s %d times, want only in the rename:\n%s", target, count, prepareScript)
		}
	}
}
