// Package metering reads token usage out of model API responses as they pass
// through the egress proxy, and posts it to the usage sink. A streamed
// response is parsed event by event as it goes by; nothing waits on it.
package metering

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Provider is a model API format.
type Provider string

const (
	Anthropic Provider = "anthropic"
	OpenAI    Provider = "openai"
)

// Metered reports whether a request to provider's path spends tokens and is
// metered. Allowed reports whether it is forwarded at all: metered paths and
// the token-free ones. Matching ignores the query; the path must already be
// canonical.
func Metered(p Provider, method, path string) bool {
	if method != "POST" {
		return false
	}
	switch p {
	case Anthropic:
		return path == "/v1/messages"
	case OpenAI:
		return path == "/v1/chat/completions" || path == "/v1/responses"
	}
	return false
}

// Allowed is Metered plus the paths that spend nothing.
func Allowed(p Provider, method, path string) bool {
	if Metered(p, method, path) {
		return true
	}
	switch {
	case method == "GET" && path == "/v1/models":
		return true
	case p == Anthropic && method == "POST" && path == "/v1/messages/count_tokens":
		return true
	}
	return false
}

// Usage is normalised token usage: all input is Input + CacheRead +
// CacheCreation.
type Usage struct {
	Model         string
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	// Complete is true once the response's final usage was seen.
	Complete bool
}

// maxBufferedBody bounds what is kept of a non-streamed response to parse
// its usage; a larger one is metered as incomplete.
const maxBufferedBody = 4 << 20

// maxEventLine bounds one streamed event line; longer lines are skipped.
const maxEventLine = 1 << 20

// Reader passes a response body through unchanged while reading its usage.
type Reader struct {
	src      io.Reader
	provider Provider
	stream   bool

	// Non-streamed: the body so far, up to maxBufferedBody.
	body     bytes.Buffer
	overflow bool
	// Streamed: the partial line not yet ended.
	line    []byte
	skipped bool

	usage Usage
}

// NewReader wraps a response body. stream is true for text/event-stream.
func NewReader(src io.Reader, p Provider, stream bool) *Reader {
	return &Reader{src: src, provider: p, stream: stream}
}

func (r *Reader) Read(b []byte) (int, error) {
	n, err := r.src.Read(b)
	if n > 0 {
		r.observe(b[:n])
	}
	if err == io.EOF && !r.stream {
		r.parseBody()
	}
	return n, err
}

// Usage is what was read so far; call it once the body is done.
func (r *Reader) Usage() Usage {
	if !r.stream && !r.usage.Complete {
		r.parseBody()
	}
	return r.usage
}

func (r *Reader) observe(b []byte) {
	if !r.stream {
		if r.overflow || r.body.Len()+len(b) > maxBufferedBody {
			r.overflow = true
			r.body.Reset()
			return
		}
		r.body.Write(b)
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			r.appendLine(b)
			return
		}
		r.appendLine(b[:i])
		if !r.skipped {
			r.event(r.line)
		}
		r.line, r.skipped = r.line[:0], false
		b = b[i+1:]
	}
}

func (r *Reader) appendLine(b []byte) {
	if r.skipped || len(r.line)+len(b) > maxEventLine {
		r.skipped, r.line = true, r.line[:0]
		return
	}
	r.line = append(r.line, b...)
}

func (r *Reader) parseBody() {
	if r.overflow || r.body.Len() == 0 || r.usage.Complete {
		return
	}
	var v wire
	if json.Unmarshal(r.body.Bytes(), &v) != nil {
		return
	}
	r.apply(&v, true)
}

// event handles one SSE line; only "data:" lines carry usage.
func (r *Reader) event(line []byte) {
	s := strings.TrimSuffix(string(line), "\r")
	data, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return
	}
	data = strings.TrimSpace(data)
	// Usage and the model ride on few events; most are text deltas, which are
	// not decoded at all.
	if data == "" || data == "[DONE]" || (!strings.Contains(data, `"usage"`) && !strings.Contains(data, `"model"`)) {
		return
	}
	var v wire
	if json.Unmarshal([]byte(data), &v) != nil {
		return
	}
	switch r.provider {
	case Anthropic:
		switch v.Type {
		case "message_start":
			if v.Message != nil {
				r.apply(v.Message, false)
			}
		case "message_delta":
			// Cumulative: the last one holds the final counts.
			r.apply(&v, true)
		}
	case OpenAI:
		switch {
		case v.Type == "response.completed" && v.Response != nil:
			r.apply(v.Response, true)
		case v.Usage != nil:
			// Chat completions send usage on the final chunk only.
			r.apply(&v, true)
		}
	}
}

func (r *Reader) apply(v *wire, final bool) {
	if v.Model != "" {
		r.usage.Model = v.Model
	}
	u := v.Usage
	if u == nil {
		return
	}
	switch r.provider {
	case Anthropic:
		// message_delta repeats only what changed; keep the rest.
		if u.InputTokens != nil {
			r.usage.Input = *u.InputTokens
		}
		if u.OutputTokens != nil {
			r.usage.Output = *u.OutputTokens
		}
		if u.CacheReadInputTokens != nil {
			r.usage.CacheRead = *u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens != nil {
			r.usage.CacheCreation = *u.CacheCreationInputTokens
		}
	case OpenAI:
		input := deref(u.PromptTokens) + deref(u.InputTokens)
		cached := int64(0)
		if u.PromptTokensDetails != nil {
			cached += deref(u.PromptTokensDetails.CachedTokens)
		}
		if u.InputTokensDetails != nil {
			cached += deref(u.InputTokensDetails.CachedTokens)
		}
		r.usage.Input = max(input-cached, 0)
		r.usage.CacheRead = cached
		r.usage.CacheCreation = 0
		r.usage.Output = deref(u.CompletionTokens) + deref(u.OutputTokens)
	}
	if final {
		r.usage.Complete = true
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// wire covers every response shape metered here.
type wire struct {
	Type     string     `json:"type"`
	Model    string     `json:"model"`
	Usage    *wireUsage `json:"usage"`
	Message  *wire      `json:"message"`
	Response *wire      `json:"response"`
}

type wireUsage struct {
	// Anthropic, and OpenAI Responses.
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	InputTokensDetails       *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// OpenAI chat completions.
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// RequestsBackground reports whether an OpenAI Responses request asks to run
// in the background, or cannot be read to tell.
func RequestsBackground(body []byte) bool {
	var v struct {
		Background *bool `json:"background"`
	}
	if json.Unmarshal(body, &v) != nil {
		return true
	}
	return v.Background != nil && *v.Background
}

// RequestModel reads the model a request body names, for a record whose
// response named none.
func RequestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &v) == nil {
		return v.Model
	}
	return ""
}
