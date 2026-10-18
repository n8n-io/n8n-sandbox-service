package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/codedaemon"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const codeProxyTestWorker = `#!/bin/sh
echo '{"type":"ready"}' >&3
read -r line <&3
dir=$(printf '%s' "$line" | sed -n 's/.*"dir":"\([^"]*\)".*/\1/p')
echo '{"type":"rpc","callId":"c1","name":"helpers.httpRequest","params":[]}' >&3
read -r res <&3
echo "rpc:$res"
cat "$dir/in/task.json" > "$dir/out/result.json"
echo '{"type":"done"}' >&3
sleep 5
`

// The runner chain must pass the streamed task response and the RPC result
// through its middleware unchanged, and must not pass credentials to the
// daemon.
func TestCodeProxyTaskRPCAndOutputs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(script, []byte(codeProxyTestWorker), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := codedaemon.New(codedaemon.Config{
		WorkDir: filepath.Join(dir, "work"), Runtime: "test", Slots: 1, TaskMemoryMB: 64,
		SpareWorkers: true, OutputTTL: time.Minute, WorkerEnv: []string{"PATH=" + os.Getenv("PATH")},
		AllowLocalClients: true,
	}, codedaemon.RuntimeSpec{Version: "9", Command: []string{"/bin/sh", script}})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		_ = d.Serve(ctx, lis)
		close(served)
	}()
	// Stop the daemon before the next test, which can capture the logs.
	defer func() {
		cancel()
		<-served
	}()

	// A recorder in front of the daemon shows the headers that reach it.
	daemonURL, _ := url.Parse("http://" + lis.Addr().String())
	toDaemon := httputil.NewSingleHostReverseProxy(daemonURL)
	toDaemon.FlushInterval = -1
	leaked := make(chan string, 16)
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Api-Key", "Authorization", "Cookie"} {
			if r.Header.Get(h) != "" {
				leaked <- r.URL.Path + " " + h
			}
		}
		toDaemon.ServeHTTP(w, r)
	}))
	defer recorder.Close()

	rt := &fakeRuntime{daemonURL: recorder.URL}
	mux := http.NewServeMux()
	registerCodeRoutes(mux, rt)
	var handler http.Handler = mux
	handler = metrics.HTTPMiddleware(metrics.NewRunnerRecorder(true))(handler)
	handler = LoggingMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	base := srv.URL + "/code/sandboxes/" + proxyTestSandboxID

	// Wait for the daemon through the proxy.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon not healthy through the proxy")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	sw, _ := mw.CreateFormField("spec")
	_, _ = sw.Write([]byte(`{"taskId":"px1","timeoutMs":5000}`))
	fw, _ := mw.CreateFormFile("input", "task.json")
	_, _ = fw.Write([]byte(`{"hello":"world"}`))
	_ = mw.Close()
	resp, err := http.Post(base+"/tasks", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		all, _ := io.ReadAll(resp.Body)
		t.Fatalf("task status %d: %s", resp.StatusCode, all)
	}
	var sawRPC, sawPong, sawDone bool
	var all []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		all = append(all, line)
		var ev struct{ Type, CallID string }
		_ = json.Unmarshal([]byte(line), &ev)
		if ev.Type == "rpc" {
			// The stream is not buffered: the result goes back while the
			// task waits for it.
			sawRPC = true
			req, _ := http.NewRequest(http.MethodPost, base+"/tasks/px1/rpc/"+ev.CallID,
				strings.NewReader(`{"ok":true,"data":"pong"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "secret")
			req.Header.Set("Authorization", "Bearer secret")
			rres, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			rres.Body.Close()
			if rres.StatusCode != http.StatusNoContent {
				t.Fatalf("rpc result status %d", rres.StatusCode)
			}
		}
		if strings.Contains(line, `"stdout"`) && strings.Contains(line, "pong") {
			sawPong = true
		}
		if strings.Contains(line, `"type":"done"`) && strings.Contains(line, `"hello":"world"`) {
			sawDone = true
		}
	}
	resp.Body.Close()
	if !sawRPC || !sawPong || !sawDone {
		t.Fatalf("events: %s", strings.Join(all, "\n"))
	}
	select {
	case h := <-leaked:
		t.Fatalf("credential header reached the daemon: %s", h)
	default:
	}

	resp, err = http.Get(base + "/tasks/px1/outputs/result.json")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(out, []byte("world")) {
		t.Fatalf("output %d %s", resp.StatusCode, out)
	}
}
