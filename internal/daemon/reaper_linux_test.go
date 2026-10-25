package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReaperReapsOrphans runs the reaper in a child process made a subreaper,
// standing in for PID 1: a command that double-forks and exits must leave no
// zombie behind, and the command's own exit code must still come back.
func TestReaperReapsOrphans(t *testing.T) {
	if os.Getenv("REAPER_HELPER") == "1" {
		reaperHelper()
		return
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestReaperReapsOrphans$")
	helper.Env = append(os.Environ(), "REAPER_HELPER=1")
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "exit=3 zombies=0") {
		t.Fatalf("unexpected helper output:\n%s", out)
	}
}

func reaperHelper() {
	const prSetChildSubreaper = 36
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		fmt.Println("prctl:", e)
		os.Exit(1)
	}
	activeReaper = newReaper()
	go activeReaper.run()

	cmd := exec.Command("/bin/sh", "-c", "(sleep 0.1 &); (sleep 0.1 &); exit 3")
	if err := cmd.Start(); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}
	code, _, err := waitProcess(cmd.Process)
	if err != nil {
		fmt.Println("wait:", err)
		os.Exit(1)
	}
	time.Sleep(2 * time.Second)
	fmt.Printf("exit=%d zombies=%d\n", code, zombieChildren())
}

func zombieChildren() int {
	entries, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range entries {
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		s := string(b)
		if strings.Contains(s, fmt.Sprintf("\nPPid:\t%d\n", os.Getpid())) && strings.Contains(s, "\nState:\tZ") {
			n++
		}
	}
	return n
}
