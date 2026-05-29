package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

const (
	execMaxRetries       = 3
	execMaxJSONBodyBytes = 1 << 20
	execRetryBaseBackoff = 50 * time.Millisecond
)

type execRequestBody struct {
	Command   string            `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
	ExecID    string            `json:"exec_id,omitempty"`
}

type ndjsonEvent struct {
	Seq  *uint64 `json:"seq,omitempty"`
	Type string  `json:"type"`
}

func isTerminalEvent(typ string) bool {
	return typ == "exit" || typ == "error"
}

// ExecProxyHandler returns a handler that proxies POST /sandboxes/{id}/executions
// to the sandbox daemon with automatic mid-stream retry. If the daemon connection
// drops before a terminal event, the handler resumes via the daemon's
// GET /executions/{exec_id}?follow=true&after=<seq> endpoint.
func ExecProxyHandler(mgr ContainerManager, cfg *config.Config) http.HandlerFunc {
	client := &http.Client{}

	return func(w http.ResponseWriter, r *http.Request) {
		daemonBaseURL, ok := resolveDaemonURL(w, r, mgr)
		if !ok {
			return
		}

		rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, execMaxJSONBodyBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
			} else {
				writeError(w, http.StatusBadRequest, "failed to read request body")
			}
			return
		}

		var body execRequestBody
		if err := json.Unmarshal(rawBody, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		if body.ExecID == "" {
			body.ExecID = uuid.New().String()
		}
		execID := body.ExecID

		encoded, err := json.Marshal(body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encode request body")
			return
		}

		daemonURL := daemonBaseURL + "/executions"
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, daemonURL, bytes.NewReader(encoded))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		upReq.Header.Set("Content-Type", "application/json")

		upResp, err := client.Do(upReq)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "daemon temporarily unavailable")
			return
		}
		defer upResp.Body.Close()

		if upResp.StatusCode != http.StatusOK {
			w.Header().Set("Content-Type", upResp.Header.Get("Content-Type"))
			w.WriteHeader(upResp.StatusCode)
			io.Copy(w, upResp.Body)
			return
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Exec-Id", execID)
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		flush()

		var lastSeq *uint64
		completed := streamNDJSON(upResp.Body, w, flush, &lastSeq)
		if completed {
			return
		}
		upResp.Body.Close()

		for attempt := range execMaxRetries {
			backoff := execRetryBaseBackoff << attempt
			select {
			case <-r.Context().Done():
				return
			case <-time.After(backoff):
			}

			resumeURL := fmt.Sprintf("%s/executions/%s?follow=true", daemonBaseURL, execID)
			if lastSeq != nil {
				resumeURL += fmt.Sprintf("&after=%d", *lastSeq)
			}
			resumeReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, resumeURL, nil)
			if err != nil {
				slog.Warn("exec proxy: failed to build resume request", "exec_id", execID, "err", err)
				continue
			}

			resumeResp, err := client.Do(resumeReq)
			if err != nil {
				slog.Warn("exec proxy: resume request failed", "exec_id", execID, "attempt", attempt+1, "err", err)
				continue
			}

			if resumeResp.StatusCode != http.StatusOK {
				resumeResp.Body.Close()
				slog.Warn("exec proxy: resume returned non-200", "exec_id", execID, "attempt", attempt+1, "status", resumeResp.StatusCode)
				continue
			}

			completed = streamNDJSON(resumeResp.Body, w, flush, &lastSeq)
			resumeResp.Body.Close()
			if completed {
				return
			}
		}

		slog.Error("exec proxy: retries exhausted", "exec_id", execID, "last_seq", lastSeq)
	}
}

// streamNDJSON reads NDJSON lines from src and writes them to dst, flushing
// after each line. It updates lastSeq with the highest sequence number seen
// and returns true if a terminal event (exit or error) was forwarded.
func streamNDJSON(src io.Reader, dst io.Writer, flush func(), lastSeq **uint64) bool {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false
		}

		seq, typ, ok := execEventPrefix(line)
		if ok {
			*lastSeq = &seq
		}

		dst.Write(line)
		flush()

		if isTerminalEvent(typ) {
			return true
		}
	}
}

func execEventPrefix(line []byte) (uint64, string, bool) {
	// The daemon marshals exec events with seq and type first, so the proxy can
	// track resume state without JSON-decoding every streamed event.
	const seqPrefix = `{"seq":`
	if !bytes.HasPrefix(line, []byte(seqPrefix)) {
		return 0, "", false
	}

	rest := line[len(seqPrefix):]
	comma := bytes.IndexByte(rest, ',')
	if comma <= 0 {
		return 0, "", false
	}

	seq, err := strconv.ParseUint(string(rest[:comma]), 10, 64)
	if err != nil {
		return 0, "", false
	}

	const typePrefix = `,"type":"`
	rest = rest[comma:]
	if !bytes.HasPrefix(rest, []byte(typePrefix)) {
		return seq, "", true
	}

	rest = rest[len(typePrefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return seq, "", true
	}
	return seq, string(rest[:end]), true
}
