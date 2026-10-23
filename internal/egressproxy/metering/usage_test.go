package metering

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixtures in each provider's wire format.
const (
	anthropicJSON = `{"id":"msg_1","type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}],
"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}`

	anthropicSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":34}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	openAIChatJSON = `{"id":"c1","object":"chat.completion","model":"gpt-6","choices":[],
"usage":{"prompt_tokens":1212,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":1000}}}`

	openAIChatSSE = `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-6","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-6","choices":[],"usage":{"prompt_tokens":1212,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":1000}}}` + "\n\n" +
		"data: [DONE]\n\n"

	openAIResponsesJSON = `{"id":"r1","object":"response","model":"gpt-6","output":[],
"usage":{"input_tokens":1212,"output_tokens":34,"input_tokens_details":{"cached_tokens":1000},"output_tokens_details":{"reasoning_tokens":20}}}`

	openAIResponsesSSE = "event: response.created\n" + `data: {"type":"response.created","response":{"id":"r1","model":"gpt-6"}}` + "\n\n" +
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"r1","model":"gpt-6","usage":{"input_tokens":1212,"output_tokens":34,"input_tokens_details":{"cached_tokens":1000}}}}` + "\n\n"
)

// readInChunks passes body through a Reader in small pieces, as a network
// would, and checks it arrives unchanged.
func readInChunks(t *testing.T, body string, p Provider, stream bool) Usage {
	t.Helper()
	r := NewReader(&chunked{s: body, n: 7}, p, stream)
	out, err := io.ReadAll(r)
	if err != nil || string(out) != body {
		t.Fatalf("body changed in passing: err=%v", err)
	}
	return r.Usage()
}

type chunked struct {
	s string
	n int
}

func (c *chunked) Read(b []byte) (int, error) {
	if c.s == "" {
		return 0, io.EOF
	}
	n := min(len(b), c.n, len(c.s))
	copy(b, c.s[:n])
	c.s = c.s[n:]
	return n, nil
}

func TestUsageFromEveryFormat(t *testing.T) {
	anthropic := Usage{Model: "claude-opus-5-5", Input: 12, Output: 34, CacheRead: 1000, CacheCreation: 200, Complete: true}
	openai := Usage{Model: "gpt-6", Input: 212, Output: 34, CacheRead: 1000, Complete: true}
	for _, tc := range []struct {
		name   string
		body   string
		p      Provider
		stream bool
		want   Usage
	}{
		{"anthropic json", anthropicJSON, Anthropic, false, anthropic},
		{"anthropic sse", anthropicSSE, Anthropic, true, anthropic},
		{"openai chat json", openAIChatJSON, OpenAI, false, openai},
		{"openai chat sse", openAIChatSSE, OpenAI, true, openai},
		{"openai responses json", openAIResponsesJSON, OpenAI, false, openai},
		{"openai responses sse", openAIResponsesSSE, OpenAI, true, openai},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readInChunks(t, tc.body, tc.p, tc.stream); got != tc.want {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A stream cut off before its final usage is a lower bound, marked incomplete.
func TestUsageOfCutStream(t *testing.T) {
	cut := anthropicSSE[:strings.Index(anthropicSSE, "event: message_delta")]
	got := readInChunks(t, cut, Anthropic, true)
	if got.Complete || got.Input != 12 || got.Output != 1 {
		t.Fatalf("usage = %+v", got)
	}
	if got := readInChunks(t, `{"error":{"type":"overloaded"}}`, Anthropic, false); got.Complete {
		t.Fatalf("error body usage = %+v", got)
	}
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct {
		p                Provider
		method, path     string
		metered, allowed bool
	}{
		{Anthropic, "POST", "/v1/messages", true, true},
		{Anthropic, "POST", "/v1/messages/count_tokens", false, true},
		{Anthropic, "POST", "/v1/messages/batches", false, false},
		{Anthropic, "GET", "/v1/models", false, true},
		{OpenAI, "POST", "/v1/chat/completions", true, true},
		{OpenAI, "POST", "/v1/responses", true, true},
		{OpenAI, "POST", "/v1/completions", false, false},
		{OpenAI, "POST", "/v1/batches", false, false},
		{OpenAI, "GET", "/v1/realtime", false, false},
	} {
		if m, a := Metered(tc.p, tc.method, tc.path), Allowed(tc.p, tc.method, tc.path); m != tc.metered || a != tc.allowed {
			t.Errorf("%s %s %s: metered %v allowed %v", tc.p, tc.method, tc.path, m, a)
		}
	}
}

// The sink gets every record once acknowledged; a 503 is retried with the
// same batch, a 400 dropped.
func TestSinkDelivers(t *testing.T) {
	var mu sync.Mutex
	var batches []batch
	fail := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var b batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, b)
		if fail > 0 {
			fail--
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := NewSink(srv.URL, "tok", "runner-1")
	for i := 0; i < 3; i++ {
		s.Add(Record{RecordID: NewRecordID(), SandboxID: "sb", InputTokens: int64(i)})
	}
	if more := s.deliverOne(context.Background()); more {
		t.Fatal("one batch expected")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || batches[0].BatchID != batches[1].BatchID || len(batches[1].Records) != 3 || batches[1].RunnerID != "runner-1" {
		t.Fatalf("batches = %+v", batches)
	}
}

func TestSinkDropsRefusedBatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	s := NewSink(srv.URL, "", "r")
	s.Add(Record{RecordID: "x"})
	start := time.Now()
	s.deliverOne(context.Background())
	if calls != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("calls = %d", calls)
	}
}

// BenchmarkStreamOverhead is what metering adds to a streamed response, per
// request: a few hundred events passed through the Reader.
func BenchmarkStreamOverhead(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(strings.SplitAfter(anthropicSSE, "\n\n")[0])
	for i := 0; i < 300; i++ {
		sb.WriteString("event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"some generated text here"}}` + "\n\n")
	}
	sb.WriteString(anthropicSSE[strings.Index(anthropicSSE, "event: message_delta"):])
	body := sb.String()
	buf := make([]byte, 4096)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		r := NewReader(strings.NewReader(body), Anthropic, true)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
		if !r.Usage().Complete {
			b.Fatal("usage not read")
		}
	}
}

// One sandbox cannot push the others' records out of the queue.
func TestSinkCapsEachSandbox(t *testing.T) {
	s := NewSink("http://127.0.0.1:1", "", "r")
	for i := 0; i < maxQueuedPerSandbox+5; i++ {
		s.Add(Record{SandboxID: "noisy"})
	}
	s.Add(Record{SandboxID: "quiet"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) != maxQueuedPerSandbox+1 || s.perSbx["quiet"] != 1 || s.perSbx["noisy"] != maxQueuedPerSandbox {
		t.Fatalf("queue %d, per sandbox %v", len(s.queue), s.perSbx)
	}
}

// FuzzReader: metering never changes what passes through, and never panics.
func FuzzReader(f *testing.F) {
	f.Add([]byte(anthropicSSE), true)
	f.Add([]byte(openAIChatJSON), false)
	f.Fuzz(func(t *testing.T, body []byte, stream bool) {
		for _, p := range []Provider{Anthropic, OpenAI} {
			r := NewReader(&chunked{s: string(body), n: 5}, p, stream)
			out, _ := io.ReadAll(r)
			if string(out) != string(body) {
				t.Fatal("body changed in passing")
			}
			u := r.Usage()
			if u.Input < 0 || u.CacheRead < 0 {
				t.Fatalf("negative usage %+v", u)
			}
		}
	})
}
