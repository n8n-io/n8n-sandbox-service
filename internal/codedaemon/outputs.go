package codedaemon

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultOutputTTL     = 5 * time.Minute
	defaultRetainedBytes = 512 << 20
	maxRetainedTasks     = 256 // each retained output set owns a directory descriptor
	outputWriteTimeout   = 2 * time.Minute
)

type retainedOutputs struct {
	dir       string
	root      *os.Root
	files     map[string]outputEntry
	expiresAt time.Time
	bytes     int64
	timer     *time.Timer
}

type outputStore struct {
	mu              sync.Mutex
	tasks           map[string]*retainedOutputs // nil after shutdown
	bytes, maxBytes int64
}

// add takes ownership of root and dir only on success.
func (s *outputStore) add(id, dir string, root *os.Root, entries []outputEntry, ttl time.Duration) (time.Time, bool) {
	outputs := &retainedOutputs{dir: dir, root: root, files: make(map[string]outputEntry, len(entries))}
	for _, entry := range entries {
		outputs.files[entry.Path] = entry
		outputs.bytes += entry.Size
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil || len(s.tasks) >= maxRetainedTasks || outputs.bytes > s.maxBytes-s.bytes {
		return time.Time{}, false
	}
	outputs.expiresAt = time.Now().Add(ttl)
	s.tasks[id] = outputs
	s.bytes += outputs.bytes
	outputs.timer = time.AfterFunc(ttl, func() { s.remove(id) })
	return outputs.expiresAt.UTC(), true
}

func (s *outputStore) open(id, path string) (*os.File, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outputs := s.tasks[id]
	if outputs == nil || !time.Now().Before(outputs.expiresAt) {
		return nil, 0
	}
	entry, ok := outputs.files[path]
	if !ok {
		return nil, 0
	}
	f, err := openOutput(outputs.root, entry)
	if err != nil {
		return nil, 0
	}
	// Expiry can unlink the file after we release the lock; an open descriptor
	// remains readable for the download already in progress.
	return f, entry.Size
}

func (s *outputStore) remove(id string) {
	s.mu.Lock()
	outputs := s.tasks[id]
	if outputs != nil {
		delete(s.tasks, id)
		s.bytes -= outputs.bytes
		outputs.timer.Stop()
	}
	s.mu.Unlock()
	if outputs != nil {
		outputs.cleanup()
	}
}

func (s *outputStore) close() {
	s.mu.Lock()
	tasks := s.tasks
	s.tasks, s.bytes = nil, 0
	s.mu.Unlock()
	for _, outputs := range tasks {
		outputs.timer.Stop()
		outputs.cleanup()
	}
}

func (o *retainedOutputs) cleanup() {
	_ = o.root.Close()
	_ = os.RemoveAll(o.dir)
}

func (d *Daemon) handleOutput(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("taskId"), r.PathValue("path")
	if !taskIDPattern.MatchString(id) || !validFilePath(path) {
		http.NotFound(w, r)
		return
	}
	f, size := d.outputs.open(id, path)
	if f == nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(outputWriteTimeout))
	defer rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.CopyN(w, f, size)
	}
}
