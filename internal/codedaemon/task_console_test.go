package codedaemon

import (
	"slices"
	"strings"
	"testing"
	"unsafe"
)

var truncatedNotice = consoleEntry{Stream: "stderr", Text: "Console output truncated"}

func TestConsoleEntryLimit(t *testing.T) {
	c := newTaskConsole(2, 100)
	for _, text := range []string{"a", "b", "c", "d"} {
		c.add("stdout", text)
	}
	want := []consoleEntry{{Stream: "stdout", Text: "a"}, truncatedNotice}
	if !slices.Equal(c.entries, want) {
		t.Fatalf("entries = %+v, want %+v", c.entries, want)
	}
}

func TestConsoleByteLimit(t *testing.T) {
	c := newTaskConsole(10, 6)
	c.add("stdout", "abc")
	c.add("stderr", "dé€") // 1 + 2 + 3 bytes; only "dé" fits
	c.add("stdout", "x")
	want := []consoleEntry{{Stream: "stdout", Text: "abc"}, {Stream: "stderr", Text: "dé"}, truncatedNotice}
	if !slices.Equal(c.entries, want) || c.bytesRead != 6 {
		t.Fatalf("entries = %+v (%d bytes), want %+v", c.entries, c.bytesRead, want)
	}
}

func TestConsoleExactBudget(t *testing.T) {
	c := newTaskConsole(2, 3)
	c.add("stdout", "a")
	c.add("stdout", "bc")
	if c.truncated || len(c.entries) != 2 {
		t.Fatalf("output within limits was truncated: %+v", c.entries)
	}
}

func TestConsoleAfterExhaustedBudget(t *testing.T) {
	c := newTaskConsole(10, 3)
	c.add("stdout", "abc")
	c.add("stdout", "d")
	want := []consoleEntry{{Stream: "stdout", Text: "abc"}, truncatedNotice}
	if !slices.Equal(c.entries, want) {
		t.Fatalf("entries = %+v, want %+v", c.entries, want)
	}
}

func TestConsoleEntryAndByteLimitsInOneCall(t *testing.T) {
	c := newTaskConsole(2, 4)
	c.add("stdout", "ab")
	c.add("stdout", "cdef") // "cd" fits the bytes but fills the last entry
	want := []consoleEntry{{Stream: "stdout", Text: "ab"}, truncatedNotice}
	if !slices.Equal(c.entries, want) {
		t.Fatalf("entries = %+v, want %+v", c.entries, want)
	}
}

func TestConsoleIgnoresUnknownStream(t *testing.T) {
	c := newTaskConsole(1, 100)
	c.add("other", "text")
	if len(c.entries) != 0 {
		t.Fatalf("entries = %+v", c.entries)
	}
}

func TestConsoleTruncationCopiesText(t *testing.T) {
	c := newTaskConsole(2, 4096)
	text := strings.Repeat("x", 1<<19)
	c.add("stdout", text)
	if got := c.entries[0].Text; got != text[:4096] || unsafe.StringData(got) == unsafe.StringData(text) {
		t.Fatal("expected a detached copy of the first 4096 bytes")
	}
}
