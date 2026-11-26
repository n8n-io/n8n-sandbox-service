package codedaemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestControlMessageLimit(t *testing.T) {
	var logs syncBuffer
	defaultLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(defaultLogger) })
	d := testDaemon(t, Config{})
	result := func(id string) (workerMessage, bool) {
		t.Helper()
		x, err := d.dispatch(t.Context(), id, d.cfg.WorkDir, d.taskTimeout(0))
		if err != nil {
			t.Fatal(err)
		}
		defer x.close()
		select {
		case msg, ok := <-x.result:
			return msg, ok
		case <-time.After(5 * time.Second):
			t.Fatal("missing runtime result")
			return workerMessage{}, false
		}
	}
	if msg, ok := result("largest"); !ok || msg.Type != "error" {
		t.Fatalf("runtime did not survive the largest control message: %q", msg.Type)
	}
	if _, ok := result("oversized"); ok {
		t.Fatal("runtime survived a message over the limit")
	}
	if want := fmt.Sprintf("control message over %d bytes", maxControlMessage); !strings.Contains(logs.String(), want) {
		t.Fatalf("log does not say why the runtime stopped: %q", logs.String())
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime stopped but health probe returned %d", probe.Code)
	}
}
