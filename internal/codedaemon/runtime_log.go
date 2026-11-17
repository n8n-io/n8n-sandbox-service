// This file samples the runtime's raw stdout and stderr into the daemon log.
package codedaemon

import (
	"bytes"
	"log/slog"
)

const defaultRuntimeLogWrites = 16

// runtimeLogger forwards a limited sample of the runtime's process output to
// the daemon log. This output is not attributed to a task, so it has its own
// budget.
type runtimeLogger struct {
	queue     chan runtimeLogLine
	maxWrites int // writes sampled per stream
}

type runtimeLogLine struct {
	stream string // "stdout" or "stderr"
	text   string
}

// newRuntimeLogger starts logging queued lines until close is called. It
// samples at most maxWrites writes per stream.
func newRuntimeLogger(maxWrites int) *runtimeLogger {
	l := &runtimeLogger{queue: make(chan runtimeLogLine, 32), maxWrites: maxWrites}
	go func() {
		for line := range l.queue {
			slog.Info("code runtime output", "stream", line.stream, "text", line.text)
		}
	}()
	return l
}

// writer returns the writer for one stream. Each writer must have a single
// caller at a time, as exec.Cmd guarantees for Stdout and Stderr.
func (l *runtimeLogger) writer(stream string) *runtimeLogWriter {
	return &runtimeLogWriter{stream: stream, queue: l.queue, maxWrites: l.maxWrites}
}

// close stops logging. Call it only after all writers are done, such as after exec.Cmd.Wait.
func (l *runtimeLogger) close() {
	close(l.queue)
}

type runtimeLogWriter struct {
	stream    string // "stdout" or "stderr"
	queue     chan<- runtimeLogLine
	maxWrites int
	writes    int    // counts writes that use the budget, even if the queue is full
	oom       bool   // whether stderr contained the heap OOM marker, even beyond the log budget
	tail      []byte // recent stderr bytes kept to match a marker split across writes
}

// Write samples at most the first maxWrites writes and 1 KiB per write,
// dropping output rather than blocking the runtime when the log queue is full.
func (w *runtimeLogWriter) Write(p []byte) (int, error) {
	if w.stream == "stderr" && !w.oom {
		const marker = "heap out of memory"
		b := append(w.tail, p...)
		w.oom = bytes.Contains(b, []byte(marker))
		w.tail = append(w.tail[:0], b[max(0, len(b)-len(marker)+1):]...)
	}
	if w.writes < w.maxWrites {
		w.writes++
		select {
		case w.queue <- runtimeLogLine{stream: w.stream, text: string(p[:min(len(p), 1024)])}:
		default:
		}
	}
	return len(p), nil
}
