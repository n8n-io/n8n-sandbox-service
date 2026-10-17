package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// The runner chain must pass the channel WebSocket and the streamed task
// response through its middleware unchanged.
func TestCodeProxyChannelTaskAndOutputs(t *testing.T) {
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

	rt := &fakeRuntime{daemonURL: "http://" + lis.Addr().String()}
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

	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	conn, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(base, "http")+"/channel", nil)
	if err != nil {
		t.Fatalf("channel through proxy: %v", err)
	}
	defer conn.CloseNow()
	if _, data, err := conn.Read(wctx); err != nil || !bytes.Contains(data, []byte("channel:ready")) {
		t.Fatalf("ready: %s %v", data, err)
	}
	go func() {
		_, data, err := conn.Read(wctx)
		if err != nil {
			return
		}
		var f struct{ TaskID, CallID string }
		_ = json.Unmarshal(data, &f)
		_ = conn.Write(wctx, websocket.MessageText, []byte(fmt.Sprintf(
			`{"type":"rpc:result","taskId":%q,"callId":%q,"ok":true,"data":"pong"}`, f.TaskID, f.CallID)))
	}()

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
	all, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("task status %d: %s", resp.StatusCode, all)
	}
	var sawPong, sawDone bool
	sc := bufio.NewScanner(bytes.NewReader(all))
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"stdout"`) && strings.Contains(line, "pong") {
			sawPong = true
		}
		if strings.Contains(line, `"type":"done"`) && strings.Contains(line, `"hello":"world"`) {
			sawDone = true
		}
	}
	if !sawPong || !sawDone {
		t.Fatalf("events: %s", all)
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
