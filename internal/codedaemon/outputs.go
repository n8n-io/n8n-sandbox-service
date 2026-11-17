package codedaemon

import (
	"errors"
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

var (
	errOutputStoreClosed    = errors.New("daemon shut down before retaining outputs")
	errOutputRetentionLimit = errors.New("output retention capacity exceeded")
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

// add takes ownership of root and dir only on success. Dispatched task IDs are
// never reused, including after expiry or deletion.
func (s *outputStore) add(id, dir string, root *os.Root, entries []outputEntry, ttl time.Duration) (time.Time, error) {
	outputs := &retainedOutputs{dir: dir, root: root, files: make(map[string]outputEntry, len(entries))}
	for _, entry := range entries {
		outputs.files[entry.Path] = entry
		outputs.bytes += entry.Size
	}
	var expired []*retainedOutputs
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		for _, outputs := range expired {
			outputs.cleanup()
		}
	}()
	if s.tasks == nil {
		return time.Time{}, errOutputStoreClosed
	}
	if !s.hasRoom(outputs.bytes) {
		// Timers may not have run yet. Expired sets are already unavailable
		// to download and should not cause a retention limit error.
		now := time.Now()
		for id, outputs := range s.tasks {
			if !now.Before(outputs.expiresAt) {
				expired = append(expired, s.detach(id))
			}
		}
		if !s.hasRoom(outputs.bytes) {
			return time.Time{}, errOutputRetentionLimit
		}
	}
	outputs.expiresAt = time.Now().Add(ttl)
	s.tasks[id] = outputs
	s.bytes += outputs.bytes
	outputs.timer = time.AfterFunc(ttl, func() { s.remove(id) })
	return outputs.expiresAt.UTC(), nil
}

// hasRoom is called with mu held.
func (s *outputStore) hasRoom(bytes int64) bool {
	return len(s.tasks) < maxRetainedTasks && bytes <= s.maxBytes-s.bytes
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
	outputs := s.detach(id)
	s.mu.Unlock()
	if outputs != nil {
		outputs.cleanup()
	}
}

// detach is called with mu held. The caller cleans up after releasing mu.
func (s *outputStore) detach(id string) *retainedOutputs {
	outputs := s.tasks[id]
	if outputs != nil {
		delete(s.tasks, id)
		s.bytes -= outputs.bytes
		outputs.timer.Stop()
	}
	return outputs
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
