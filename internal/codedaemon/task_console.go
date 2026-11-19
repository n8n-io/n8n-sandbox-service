// This file bounds the console output that the runtime attributes to each task.
package codedaemon

import (
	"strings"
	"unicode/utf8"
)

// consoleEntry holds the formatted text of one console call, or a truncation notice.
type consoleEntry struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Text   string `json:"text"`
}

// taskConsole retains a task's console output within its limits.
type taskConsole struct {
	maxEntries int
	maxBytes   int

	entries   []consoleEntry
	bytesRead int  // UTF-8 bytes of retained text, excluding the truncation notice
	truncated bool // output was dropped; later output is ignored
}

// newTaskConsole returns a console that retains at most maxEntries entries
// and maxBytes bytes of text. Both must be positive.
func newTaskConsole(maxEntries, maxBytes int) *taskConsole {
	return &taskConsole{maxEntries: maxEntries, maxBytes: maxBytes}
}

// add retains text up to the entry and byte limits. Text that crosses the
// byte budget is cut at a UTF-8 boundary. Once anything is dropped, a
// truncation notice ends the output, replacing the last entry if there is no
// room for another.
func (c *taskConsole) add(stream, text string) {
	if c.truncated || (stream != "stdout" && stream != "stderr") {
		return
	}
	if len(c.entries) == c.maxEntries {
		c.truncate()
		return
	}
	if remaining := c.maxBytes - c.bytesRead; len(text) > remaining {
		if cut := utf8Prefix(text, remaining); cut != "" {
			// Don't retain the full decoded control frame through a substring.
			c.entries = append(c.entries, consoleEntry{Stream: stream, Text: strings.Clone(cut)})
			c.bytesRead += len(cut)
		}
		c.truncate()
		return
	}
	c.entries = append(c.entries, consoleEntry{Stream: stream, Text: text})
	c.bytesRead += len(text)
}

func (c *taskConsole) truncate() {
	c.truncated = true
	notice := consoleEntry{Stream: "stderr", Text: "Console output truncated"}
	if len(c.entries) < c.maxEntries {
		c.entries = append(c.entries, notice)
	} else {
		c.entries[len(c.entries)-1] = notice
	}
}

// utf8Prefix returns the longest prefix of s of at most n bytes that does not
// split a UTF-8 sequence.
func utf8Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
