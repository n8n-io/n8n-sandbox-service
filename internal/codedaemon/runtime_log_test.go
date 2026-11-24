package codedaemon

import (
	"strings"
	"testing"
)

func TestRuntimeLogWriterSamplesMaxWrites(t *testing.T) {
	queue := make(chan runtimeLogLine, 10)
	w := &runtimeLogWriter{stream: "stdout", queue: queue, maxWrites: 2}
	for range 5 {
		if n, err := w.Write([]byte("line")); n != 4 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if len(queue) != 2 {
		t.Fatalf("sampled %d writes, want 2", len(queue))
	}
}

func TestRuntimeLogWriterCutsLongWrites(t *testing.T) {
	queue := make(chan runtimeLogLine, 1)
	w := &runtimeLogWriter{stream: "stderr", queue: queue, maxWrites: 1}
	p := []byte(strings.Repeat("a", 1024) + strings.Repeat("b", 1024))
	if n, err := w.Write(p); n != len(p) || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if line := <-queue; line.text != string(p[:1024]) {
		t.Fatalf("queued %d bytes, want the first 1024", len(line.text))
	}
}
