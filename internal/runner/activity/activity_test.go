package activity

import (
	"slices"
	"testing"
	"time"
)

func newTestTracker() (*Tracker, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	tr := New()
	tr.now = func() time.Time { return now }
	return tr, &now
}

func TestStreamCountsUntilClosed(t *testing.T) {
	tr, _ := newTestTracker()
	closeA := tr.StreamOpened("a")
	closeB := tr.StreamOpened("a")
	if !tr.Active("a") || tr.Snapshot().Streams != 2 {
		t.Fatal("two open streams not counted")
	}
	closeA()
	closeA() // a repeat is harmless
	if !tr.Active("a") {
		t.Fatal("one stream still open")
	}
	closeB()
	if tr.Active("a") || len(tr.Snapshot().Active) != 0 {
		t.Fatal("closed streams still active")
	}
}

func TestExecutionActiveUntilEndedOrDeadline(t *testing.T) {
	tr, now := newTestTracker()
	tr.ExecStarted("a", "e1", time.Hour)
	tr.ExecStarted("b", "e2", time.Minute)
	snap := tr.Snapshot()
	slices.Sort(snap.Active)
	if !slices.Equal(snap.Active, []string{"a", "b"}) || snap.Executions != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	tr.ExecEnded("a", "e1")
	if tr.Active("a") {
		t.Fatal("ended execution still active")
	}
	*now = now.Add(time.Minute + Grace + time.Second)
	if tr.Active("b") {
		t.Fatal("execution past its timeout and grace still active")
	}
}

// A repeated exec_id renews the deadline but keeps the last event seen.
func TestExecStartedAgainKeepsProgress(t *testing.T) {
	tr, now := newTestTracker()
	tr.ExecStarted("a", "e", time.Minute)
	tr.ExecProgress("a", "e", 7)
	*now = now.Add(time.Minute)
	tr.ExecStarted("a", "e", time.Minute)
	*now = now.Add(time.Minute)
	if !tr.Active("a") {
		t.Fatal("renewed execution expired")
	}
	p := tr.Unwatched()["a"]
	if len(p) != 1 || p[0].Seq == nil || *p[0].Seq != 7 {
		t.Fatalf("unwatched = %+v", p)
	}
}

func TestUnwatchedSkipsStreamedSandboxes(t *testing.T) {
	tr, _ := newTestTracker()
	tr.ExecStarted("a", "e1", time.Hour)
	tr.ExecStarted("b", "e2", time.Hour)
	closeA := tr.StreamOpened("a")
	u := tr.Unwatched()
	if _, ok := u["a"]; ok || len(u["b"]) != 1 || u["b"][0].ExecID != "e2" || u["b"][0].Seq != nil {
		t.Fatalf("unwatched = %+v", u)
	}
	closeA()
	if len(tr.Unwatched()["a"]) != 1 {
		t.Fatal("execution with its stream closed not unwatched")
	}
}

func TestForgetDropsExecutions(t *testing.T) {
	tr, _ := newTestTracker()
	tr.ExecStarted("a", "e1", time.Hour)
	tr.Forget("a")
	if tr.Active("a") {
		t.Fatal("forgotten sandbox still active")
	}
}

func TestNilTrackerTracksNothing(t *testing.T) {
	var tr *Tracker
	tr.StreamOpened("a")()
	tr.ExecStarted("a", "e", time.Hour)
	tr.ExecProgress("a", "e", 1)
	tr.ExecEnded("a", "e")
	tr.Forget("a")
	if tr.Active("a") || len(tr.Snapshot().Active) != 0 || tr.Unwatched() != nil {
		t.Fatal("nil tracker reported activity")
	}
}
