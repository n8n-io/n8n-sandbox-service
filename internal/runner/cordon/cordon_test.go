package cordon

import "testing"

func TestSetReportsChangesAndSignalsOnce(t *testing.T) {
	s := New()
	if s.Cordoned() {
		t.Fatal("new state is cordoned")
	}
	if s.Set(false) {
		t.Fatal("Set(false) on an uncordoned state reported a change")
	}
	select {
	case <-s.Changed():
		t.Fatal("Changed fired without a change")
	default:
	}

	if !s.Set(true) || !s.Cordoned() {
		t.Fatal("Set(true) did not cordon")
	}
	if s.Set(true) {
		t.Fatal("second Set(true) reported a change")
	}
	if !s.Set(false) || s.Cordoned() {
		t.Fatal("Set(false) did not uncordon")
	}

	// Two changes while nobody read leave one pending signal, not a blocked Set.
	select {
	case <-s.Changed():
	default:
		t.Fatal("Changed did not fire after a change")
	}
	select {
	case <-s.Changed():
		t.Fatal("Changed fired twice for changes made before one read")
	default:
	}
}
