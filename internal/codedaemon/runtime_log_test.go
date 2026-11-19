package codedaemon

import "testing"

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
