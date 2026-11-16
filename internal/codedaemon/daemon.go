package codedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// workerReadyTimeout bounds how long a task waits for a worker to load.
const workerReadyTimeout = 30 * time.Second

var taskIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// lane is one task lane. A lane runs at most one task at a time.
type lane struct {
	idx int
	dir string

	// Guarded by Daemon.mu.
	busy   bool
	spare  *worker
	taskID string
}

// runningTask is a task that has a lane.
type runningTask struct {
	lane   *lane
	cancel context.CancelCauseFunc
	// rpc holds the open RPC calls of the task.
	rpc *pendingCalls
}

// Daemon is the code daemon.
type Daemon struct {
	cfg     Config
	runtime RuntimeSpec
	command []string

	lanesDir   string
	outputsDir string
	trashDir   string
	// sharedDir is the working folder of the shared worker (reuse mode).
	sharedDir string

	mu       sync.Mutex
	lanes    []*lane
	running  map[string]*runningTask
	finished map[string]time.Time // taskId -> when its outputs were stored
	closed   bool
	// shared is the shared worker in reuse mode, or nil.
	shared *sharedWorker
	// sharedFailures counts shared workers in a row that exited before they
	// were ready.
	sharedFailures int

	startedAt   time.Time
	listeningAt atomic.Int64
	// workersStarted counts all worker processes that the daemon started.
	workersStarted atomic.Int64
	firstSpareAt   atomic.Int64
	firstSpareOnce sync.Once
}

// New prepares the work folders and the lanes. It does not start workers.
func New(cfg Config, rt RuntimeSpec) (*Daemon, error) {
	heapMB := cfg.TaskMemoryMB
	if cfg.ReuseWorkers {
		// One process runs the tasks of all lanes.
		heapMB = cfg.TaskMemoryMB * cfg.MaxConcurrency
	}
	d := &Daemon{
		cfg:        cfg,
		runtime:    rt,
		command:    rt.workerCommand(heapMB, cfg.TaskMemoryMB),
		lanesDir:   filepath.Join(cfg.WorkDir, "lanes"),
		outputsDir: filepath.Join(cfg.WorkDir, "outputs"),
		trashDir:   filepath.Join(cfg.WorkDir, "trash"),
		sharedDir:  filepath.Join(cfg.WorkDir, "shared"),
		running:    make(map[string]*runningTask),
		finished:   make(map[string]time.Time),
		startedAt:  time.Now(),
	}
	for _, dir := range []string{d.lanesDir, d.outputsDir, d.trashDir, d.sharedDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	for i := 0; i < cfg.MaxConcurrency; i++ {
		s := &lane{idx: i, dir: filepath.Join(d.lanesDir, strconv.Itoa(i))}
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, fmt.Errorf("create lane %d: %w", i, err)
		}
		d.lanes = append(d.lanes, s)
	}
	return d, nil
}

// Handler returns the HTTP routes of the daemon.
func (d *Daemon) Handler() http.Handler {
	return d.refuseLocalClients(d.routes())
}

// refuseLocalClients refuses connections from inside the sandbox. Only the
// runner may call the daemon. Without it, user code could run tasks under a
// task id of its choice, answer RPC calls or cancel other tasks.
// The runner connects from the bridge gateway, which is not a local address,
// and user code has no CAP_NET_RAW to fake one.
func (d *Daemon) refuseLocalClients(next http.Handler) http.Handler {
	if d.cfg.AllowLocalClients {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || isLocalAddress(ip) {
			writeError(w, http.StatusForbidden, "local clients are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalAddress reports whether ip is a loopback address or an address of
// this container.
func isLocalAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.handleHealth)
	mux.HandleFunc("GET /state", d.handleState)
	mux.HandleFunc("POST /tasks", d.handleTask)
	mux.HandleFunc("GET /tasks/{taskId}/outputs/{path...}", d.handleOutput)
	mux.HandleFunc("DELETE /tasks/{taskId}", d.handleDeleteTask)
	mux.HandleFunc("POST /tasks/{taskId}/rpc/{callId}", d.handleRPCResult)
	return mux
}

// Run serves HTTP on cfg.ListenAddr until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	lis, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	return d.Serve(ctx, lis)
}

// Serve serves HTTP on lis until ctx is done. It starts the spare workers.
func (d *Daemon) Serve(ctx context.Context, lis net.Listener) error {
	srv := &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	d.listeningAt.Store(time.Now().UnixNano())
	if d.cfg.SpareWorkers {
		if d.cfg.ReuseWorkers {
			d.startSharedEarly()
		} else {
			d.startInitialSpares()
		}
	}
	janitorCtx, stopJanitor := context.WithCancel(ctx)
	defer stopJanitor()
	go d.janitor(janitorCtx)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	select {
	case <-ctx.Done():
		d.shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		d.shutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (d *Daemon) shutdown() {
	d.mu.Lock()
	d.closed = true
	var spares []*worker
	for _, s := range d.lanes {
		if s.spare != nil {
			spares = append(spares, s.spare)
			s.spare = nil
		}
	}
	for _, rt := range d.running {
		rt.cancel(errors.New("daemon shutdown"))
	}
	if d.shared != nil {
		spares = append(spares, d.shared.w)
		d.shared = nil
	}
	d.mu.Unlock()
	for _, w := range spares {
		w.kill()
		w.release()
	}
}

// startInitialSpares starts the spare of the first lane alone, and the other
// spares after it is ready. The sandbox is ready when one spare is ready, and
// workers that load at the same time share the CPU limit of the container.
func (d *Daemon) startInitialSpares() {
	first := d.lanes[0]
	d.startSpare(first)
	if len(d.lanes) == 1 {
		return
	}
	d.mu.Lock()
	w := first.spare
	d.mu.Unlock()
	go func() {
		if w != nil {
			select {
			case <-w.ready:
			case <-w.exited:
			case <-time.After(workerReadyTimeout):
			}
		}
		for _, s := range d.lanes[1:] {
			d.startSpare(s)
		}
	}()
}

// startSpare starts a worker for the lane, so the next task does not wait for
// the runtime to load.
func (d *Daemon) startSpare(s *lane) {
	d.mu.Lock()
	// A busy lane gets its spare when the task ends (releaseLane).
	if d.closed || s.spare != nil || s.busy {
		d.mu.Unlock()
		return
	}
	w, err := startWorker(d.command, d.cfg.WorkerEnv, s.dir)
	if err == nil {
		d.workersStarted.Add(1)
	}
	if err != nil {
		d.mu.Unlock()
		slog.Error("start spare worker", "lane", s.idx, "err", err)
		return
	}
	s.spare = w
	d.mu.Unlock()

	go func() {
		select {
		case <-w.ready:
			d.firstSpareOnce.Do(func() { d.firstSpareAt.Store(time.Now().UnixNano()) })
		case <-w.exited:
			slog.Warn("spare worker exited", "lane", s.idx, "state", w.exitDescription(), "stderr", w.stderrTailString())
			d.mu.Lock()
			if s.spare == w {
				s.spare = nil
			}
			d.mu.Unlock()
			w.release()
		}
	}()
}

// takeWorker returns the spare worker of the lane or starts a new one.
func (d *Daemon) takeWorker(s *lane) (*worker, bool, error) {
	d.mu.Lock()
	w := s.spare
	s.spare = nil
	d.mu.Unlock()
	if w != nil {
		select {
		case <-w.exited:
			w.release()
		default:
			return w, true, nil
		}
	}
	w, err := startWorker(d.command, d.cfg.WorkerEnv, s.dir)
	if err == nil {
		d.workersStarted.Add(1)
	}
	return w, false, err
}

// acquireLane reserves a free lane for taskID. It returns nil if no lane is
// free.
func (d *Daemon) acquireLane(taskID string, cancel context.CancelCauseFunc) (*runningTask, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errors.New("daemon is shutting down")
	}
	if _, ok := d.running[taskID]; ok {
		return nil, errTaskExists
	}
	if _, ok := d.finished[taskID]; ok {
		return nil, errTaskExists
	}
	for _, s := range d.lanes {
		if !s.busy {
			s.busy = true
			s.taskID = taskID
			rt := &runningTask{lane: s, cancel: cancel, rpc: newPendingCalls()}
			d.running[taskID] = rt
			return rt, nil
		}
	}
	return nil, nil
}

// releaseLane cleans the lane folder and frees the lane.
func (d *Daemon) releaseLane(s *lane, taskID string, outputsStored bool) {
	// Rename first, so the lane is clean at once. Large input files are
	// deleted in the background.
	trash := filepath.Join(d.trashDir, fmt.Sprintf("%d-%d", s.idx, time.Now().UnixNano()))
	if err := os.Rename(s.dir, trash); err == nil {
		go func() { _ = os.RemoveAll(trash) }()
	} else {
		_ = os.RemoveAll(s.dir)
	}
	_ = os.MkdirAll(s.dir, 0o700)

	d.mu.Lock()
	s.busy = false
	s.taskID = ""
	delete(d.running, taskID)
	if outputsStored {
		d.finished[taskID] = time.Now()
	}
	d.mu.Unlock()

	if d.cfg.SpareWorkers && !d.cfg.ReuseWorkers {
		d.startSpare(s)
	}
}

var errTaskExists = errors.New("task id already used")

type stateResponse struct {
	Status         string `json:"status"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
	MaxConcurrency int    `json:"maxConcurrency"`
	BusyLanes      int    `json:"busyLanes"`
	SpareReady     int    `json:"spareReady"`
	SpareWorkers   bool   `json:"spareWorkers"`
	ReuseWorkers   bool   `json:"reuseWorkers"`
	WorkersStarted int64  `json:"workersStarted"`
	TaskMemoryMB   int    `json:"taskMemoryMb"`
	// Resource use of the sandbox container (its cgroup), since the start.
	// Zero if the cgroup files are not readable.
	CPUUsageUsec       int64 `json:"cpuUsageUsec"`
	MemoryPeakBytes    int64 `json:"memoryPeakBytes"`
	MemoryCurrentBytes int64 `json:"memoryCurrentBytes"`
	UptimeMs           int64 `json:"uptimeMs"`
	Timings            struct {
		DaemonReadyMs int64 `json:"daemonReady"`
		SpareReadyMs  int64 `json:"spareReady"`
	} `json:"timings"`
}

func (d *Daemon) state() (stateResponse, bool) {
	var st stateResponse
	st.Runtime = d.cfg.Runtime
	st.RuntimeVersion = d.runtime.Version
	st.MaxConcurrency = d.cfg.MaxConcurrency
	st.SpareWorkers = d.cfg.SpareWorkers
	st.ReuseWorkers = d.cfg.ReuseWorkers
	st.WorkersStarted = d.workersStarted.Load()
	st.TaskMemoryMB = d.cfg.TaskMemoryMB
	st.CPUUsageUsec, st.MemoryPeakBytes, st.MemoryCurrentBytes = readCgroupUsage(d.cfg.CgroupDir)
	st.UptimeMs = time.Since(d.startedAt).Milliseconds()

	d.mu.Lock()
	for _, s := range d.lanes {
		if s.busy {
			st.BusyLanes++
		}
		if s.spare != nil {
			select {
			case <-s.spare.ready:
				st.SpareReady++
			default:
			}
		}
	}
	if d.shared != nil {
		select {
		case <-d.shared.w.ready:
			st.SpareReady = 1
		default:
		}
	}
	d.mu.Unlock()

	if v := d.listeningAt.Load(); v > 0 {
		st.Timings.DaemonReadyMs = time.Duration(v - d.startedAt.UnixNano()).Milliseconds()
	}
	spareAt := d.firstSpareAt.Load()
	if spareAt > 0 {
		st.Timings.SpareReadyMs = time.Duration(spareAt - d.startedAt.UnixNano()).Milliseconds()
	}
	ready := d.listeningAt.Load() > 0 && (!d.cfg.SpareWorkers || spareAt > 0)
	if ready {
		st.Status = "ready"
	} else {
		st.Status = "starting"
	}
	return st, ready
}

// handleHealth answers 200 when the daemon can take a task without loading a
// runtime first: when spare workers are on, at least one must be ready.
func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	st, ready := d.state()
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, st)
}

func (d *Daemon) handleState(w http.ResponseWriter, r *http.Request) {
	st, _ := d.state()
	writeJSON(w, http.StatusOK, st)
}

// handleOutput streams one output file of a finished task.
func (d *Daemon) handleOutput(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskId")
	rel := r.PathValue("path")
	if !taskIDRegex.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if _, err := safeRelPath(rel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, fi, err := openRegular(filepath.Join(d.outputsDir, taskID), rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "output not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 256*1024)
	_, _ = copyBuffer(w, f, buf)
}

// handleDeleteTask cancels a running task, or deletes the outputs of a finished
// task.
func (d *Daemon) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskId")
	if !taskIDRegex.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	d.mu.Lock()
	rt := d.running[taskID]
	_, finished := d.finished[taskID]
	delete(d.finished, taskID)
	d.mu.Unlock()
	if rt != nil {
		rt.cancel(errCancelledByClient)
	}
	if finished {
		_ = os.RemoveAll(filepath.Join(d.outputsDir, taskID))
	}
	w.WriteHeader(http.StatusNoContent)
}

var errCancelledByClient = errors.New("task cancelled by client")

// janitor deletes outputs that nobody deleted within the output TTL.
func (d *Daemon) janitor(ctx context.Context) {
	interval := d.cfg.OutputTTL / 5
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var expired []string
			d.mu.Lock()
			for id, at := range d.finished {
				if now.Sub(at) > d.cfg.OutputTTL {
					expired = append(expired, id)
					delete(d.finished, id)
				}
			}
			d.mu.Unlock()
			for _, id := range expired {
				_ = os.RemoveAll(filepath.Join(d.outputsDir, id))
				slog.Info("deleted expired task outputs", "task_id", id)
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
