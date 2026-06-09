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

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const (
	execMaxRetries       = 3
	execMaxJSONBodyBytes = 1 << 20
	execRetryBaseBackoff = 50 * time.Millisecond
)

var (
	errExecStreamRead  = errors.New("exec stream read failed")
	errExecStreamWrite = errors.New("exec stream write failed")
	errExecEventPrefix = errors.New("exec event prefix parse failed")
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
// GET /executions/{exec_id}?follow=true&after=<seq> endpoint. We have seen
// connection drops in load tests, and this retrying here mitigates it.
func ExecProxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder) http.HandlerFunc {
	client := &http.Client{}

	return func(w http.ResponseWriter, r *http.Request) {
		daemonBaseURL, ok := resolveDaemonURL(w, r, rt, rec)
		if !ok {
			return
		}

		body, ok := readAndParseRequestBody(w, r)
		if !ok {
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
		completed, streamErr := streamNDJSON(upResp.Body, w, flush, &lastSeq)
		if completed {
			return
		}
		upResp.Body.Close()
		if !shouldRetryStreamError(r, execID, 0, streamErr, lastSeq) {
			return
		}

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

			completed, streamErr = streamNDJSON(resumeResp.Body, w, flush, &lastSeq)
			resumeResp.Body.Close()
			if completed {
				return
			}
			if !shouldRetryStreamError(r, execID, attempt+1, streamErr, lastSeq) {
				return
			}
		}

		slog.Error("exec proxy: retries exhausted", "exec_id", execID, "last_seq", lastSeqValue(lastSeq), "err", streamErr)
	}
}

func readAndParseRequestBody(w http.ResponseWriter, r *http.Request) (execRequestBody, bool) {
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, execMaxJSONBodyBytes))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
		} else {
			writeError(w, http.StatusBadRequest, "failed to read request body")
		}
		return execRequestBody{}, false
	}

	var body execRequestBody
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return execRequestBody{}, false
	}
	return body, true
}

func shouldRetryStreamError(r *http.Request, execID string, attempt int, err error, lastSeq *uint64) bool {
	if err == nil || r.Context().Err() != nil {
		return false
	}
	if !errors.Is(err, errExecStreamRead) {
		slog.Warn("exec proxy: stream failed without retry", "exec_id", execID, "attempt", attempt, "last_seq", lastSeqValue(lastSeq), "err", err)
		return false
	}

	slog.Warn("exec proxy: daemon stream interrupted; retrying", "exec_id", execID, "attempt", attempt, "last_seq", lastSeqValue(lastSeq), "err", err)
	return true
}

func lastSeqValue(seq *uint64) any {
	if seq == nil {
		return nil
	}
	return *seq
}

// streamNDJSON reads NDJSON lines from src and writes them to dst, flushing
// after each line. It updates lastSeq with the highest sequence number seen
// and returns true if a terminal event (exit or error) was forwarded.
func streamNDJSON(src io.Reader, dst io.Writer, flush func(), lastSeq **uint64) (bool, error) {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false, fmt.Errorf("%w: %w", errExecStreamRead, err)
		}

		seq, typ, err := execEventPrefix(line)
		if err != nil {
			return false, fmt.Errorf("%w: %w", errExecEventPrefix, err)
		}
		*lastSeq = &seq

		if _, err := dst.Write(line); err != nil {
			return false, fmt.Errorf("%w: %w", errExecStreamWrite, err)
		}
		flush()

		if isTerminalEvent(typ) {
			return true, nil
		}
	}
}

func execEventPrefix(line []byte) (uint64, string, error) {
	// The daemon marshals exec events with seq and type first, so the proxy can
	// track resume state without JSON-decoding every streamed event.
	const seqPrefix = `{"seq":`
	if !bytes.HasPrefix(line, []byte(seqPrefix)) {
		return 0, "", fmt.Errorf("missing %q prefix", seqPrefix)
	}

	rest := line[len(seqPrefix):]
	comma := bytes.IndexByte(rest, ',')
	if comma <= 0 {
		return 0, "", errors.New("missing seq terminator")
	}

	seq, err := strconv.ParseUint(string(rest[:comma]), 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse seq: %w", err)
	}

	const typePrefix = `,"type":"`
	rest = rest[comma:]
	if !bytes.HasPrefix(rest, []byte(typePrefix)) {
		return seq, "", fmt.Errorf("missing %q after seq", typePrefix)
	}

	rest = rest[len(typePrefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return seq, "", errors.New("unterminated type field")
	}
	return seq, string(rest[:end]), nil
}
