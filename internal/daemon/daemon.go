package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxJSONBodyBytes = 1 << 20
)

var defaultExecTimeout = 5 * time.Minute

type execRequest struct {
	Command   string            `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
	ExecID    string            `json:"exec_id,omitempty"`
}

type copyRequest struct {
	Src       string `json:"src"`
	Dest      string `json:"dest"`
	Recursive bool   `json:"recursive,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

type moveRequest struct {
	Src       string `json:"src"`
	Dest      string `json:"dest"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// Handler wraps the daemon HTTP mux and owns the exec session manager.
type Handler struct {
	mux *http.ServeMux
	sm  *SessionManager
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// Close stops background goroutines owned by the handler.
func (h *Handler) Close() {
	h.sm.Close()
}

// Run serves the daemon HTTP API until SIGTERM/SIGINT is received.
func Run(listenAddr string, baseDir string) error {
	if baseDir == "" {
		baseDir = os.Getenv("SANDBOX_DATA_DIR")
	}
	if baseDir == "" {
		baseDir = "/"
	}

	h := NewHandler(baseDir)
	defer h.Close()

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           h,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("daemon shutdown", "err", err)
		}
	}()

	slog.Info("daemon listening", "addr", listenAddr, "base_dir", baseDir)
	err := srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	return nil
}

// NewHandler exposes the in-sandbox HTTP API used by the service.
func NewHandler(baseDir string) *Handler {
	sm := NewSessionManager()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		var req execRequest
		if err := decodeJSONBody(r.Body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Command == "" {
			writeError(w, http.StatusBadRequest, "command is required")
			return
		}

		env := make([]string, 0, len(req.Env))
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}

		timeout := defaultExecTimeout
		if req.TimeoutMs > 0 {
			timeout = time.Duration(req.TimeoutMs) * time.Millisecond
		}

		sess := sm.GetOrCreate(req.ExecID, req.Command, env, req.WorkDir, timeout)

		if !sess.HasHistory(nil) {
			writeError(w, http.StatusGone, "exec session history is no longer retained")
			return
		}

		writeNdjsonHeader(w)
		write := ndjsonWriter(w)
		sess.Follow(r.Context(), nil, write)
	})

	mux.HandleFunc("GET /exec/{exec_id}", func(w http.ResponseWriter, r *http.Request) {
		after, err := parseAfterParam(r.URL.Query().Get("after"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid after parameter")
			return
		}

		execID := r.PathValue("exec_id")
		sess := sm.Get(execID)
		if sess == nil {
			writeError(w, http.StatusNotFound, "exec session not found")
			return
		}

		follow := r.URL.Query().Get("follow") == "true"

		if !sess.HasHistory(after) {
			writeError(w, http.StatusGone, "requested event history is no longer retained")
			return
		}

		// Best-effort preflight before committing the 200/NDJSON response. There is
		// still a narrow race where history can be trimmed between this check and the
		// subsequent Snapshot/Follow call, in which case the client may observe a 200
		// with an incomplete stream instead of a 410. We consider that window very
		// low probability in practice and accept it for now.
		writeNdjsonHeader(w)
		write := ndjsonWriter(w)

		if follow {
			sess.Follow(r.Context(), after, write)
		} else {
			events, ok := sess.Snapshot(after)
			if !ok {
				errData, _ := json.Marshal(newErrorResponse("requested event history is no longer retained"))
				write(errData)
				return
			}
			for _, data := range events {
				write(data)
			}
		}
	})

	mux.HandleFunc("DELETE /exec/{exec_id}", func(w http.ResponseWriter, r *http.Request) {
		execID := r.PathValue("exec_id")
		if !sm.Cancel(execID) {
			writeError(w, http.StatusNotFound, "exec session not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			path = "/"
		}

		files, err := HandleFileList(baseDir, path, r.URL.Query().Get("recursive") == "true", r.URL.Query().Get("extension"))
		if err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, files)
	})

	mux.HandleFunc("GET /files/content", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing file path")
			return
		}

		data, err := HandleFileRead(baseDir, path, 0)
		if err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("PUT /files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing file path")
			return
		}

		data, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
			return
		}

		overwrite := r.URL.Query().Get("overwrite") != "false"
		if err := HandleFileWrite(baseDir, path, data, 0, overwrite); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing file path")
			return
		}

		data, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
			return
		}

		if err := HandleFileAppend(baseDir, path, data); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing file path")
			return
		}

		if err := HandleFileDelete(baseDir, path, r.URL.Query().Get("recursive") == "true", r.URL.Query().Get("force") == "true"); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /files/copy", func(w http.ResponseWriter, r *http.Request) {
		var req copyRequest
		if err := decodeJSONBody(r.Body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Src == "" || req.Dest == "" {
			writeError(w, http.StatusBadRequest, "src and dest are required")
			return
		}

		if err := HandleFileCopy(baseDir, req.Src, req.Dest, req.Recursive, req.Overwrite); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /files/move", func(w http.ResponseWriter, r *http.Request) {
		var req moveRequest
		if err := decodeJSONBody(r.Body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Src == "" || req.Dest == "" {
			writeError(w, http.StatusBadRequest, "src and dest are required")
			return
		}

		if err := HandleFileMove(baseDir, req.Src, req.Dest, req.Overwrite); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /mkdir", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing directory path")
			return
		}

		if err := HandleFileMkdir(baseDir, path, r.URL.Query().Get("recursive") == "true"); err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("GET /stat", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "missing file path")
			return
		}

		stat, err := HandleFileStat(baseDir, path)
		if err != nil {
			writeError(w, fileOpStatusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, stat)
	})

	return &Handler{mux: mux, sm: sm}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	body, err := json.Marshal(errorResponse{Error: msg, Code: code})
	if err != nil {
		body = []byte(`{"error":"internal error","code":500}`)
		code = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json response", "err", err)
	}
}

func decodeJSONBody(body io.Reader, dst any) error {
	dec := json.NewDecoder(io.LimitReader(body, maxJSONBodyBytes))
	if err := dec.Decode(dst); err != nil {
		return err
	}

	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain a single JSON value")
		}
		return err
	}
	return nil
}

func fileOpStatusCode(err error) int {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "already exists"):
		return http.StatusConflict
	case strings.Contains(msg, "not found"), strings.Contains(msg, "no such file or directory"):
		return http.StatusNotFound
	case strings.Contains(msg, "path escapes base directory"),
		strings.Contains(msg, "source is a directory"),
		strings.Contains(msg, "destination must not be inside source directory"),
		strings.Contains(msg, "invalid argument"),
		strings.Contains(msg, "directory not empty"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeNdjsonHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func ndjsonWriter(w http.ResponseWriter) func([]byte) {
	flusher, _ := w.(http.Flusher)
	return func(data []byte) {
		if _, err := w.Write(data); err != nil {
			slog.Warn("write exec event", "err", err)
			return
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			slog.Warn("write exec event newline", "err", err)
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// parseAfterParam parses the "after" query parameter. Returns nil when the
// parameter is absent (meaning "all events"), or a pointer to the parsed value.
func parseAfterParam(v string) (*uint64, error) {
	if v == "" {
		return nil, nil
	}
	after, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return nil, err
	}
	return &after, nil
}

func parseTimeoutMs(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}
