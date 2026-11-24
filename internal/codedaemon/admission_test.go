package codedaemon

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func taskRequest(t *testing.T, spec string, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("spec", spec); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		part, err := mw.CreateFormFile("input", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/tasks", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestAdmissionAcceptsLongestTaskID(t *testing.T) {
	d := New(Config{MaxConcurrency: 1})
	r := taskRequest(t, `{"taskId":"`+strings.Repeat("x", 128)+`"}`, nil)
	if _, mr := d.admit(httptest.NewRecorder(), r); mr == nil {
		t.Fatal("spec with the longest task ID was not admitted")
	}
	d.releaseLane()
}

func TestAdmissionRejectsBeforeReadingInputPart(t *testing.T) {
	d := New(Config{MaxConcurrency: 1})
	r := taskRequest(t, `{"taskId":"first"}`, nil)
	if _, mr := d.admit(httptest.NewRecorder(), r); mr == nil {
		t.Fatal("first request was not admitted")
	}
	const boundary = "test-boundary"
	// Send only enough of the body to finish the first part. An attempt to
	// advance to the input part would have to read the withheld data.
	var body io.Reader = io.MultiReader(strings.NewReader("--"+boundary+"\r\nContent-Disposition: form-data; name=\"spec\"\r\n\r\n{\"taskId\":\"second\"}\r\n--"+boundary+"\r\n"), failRead{t})
	r = httptest.NewRequest(http.MethodPost, "/tasks", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	w := httptest.NewRecorder()
	if _, mr := d.admit(w, r); mr != nil || w.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated admission status = %d", w.Code)
	}
	d.releaseLane()
	r = taskRequest(t, `{"taskId":"second"}`, nil)
	if _, mr := d.admit(httptest.NewRecorder(), r); mr == nil {
		t.Fatal("released lane was not reusable")
	}
	d.releaseLane()
}

func TestAdmissionClosesHTTP1ConnectionWithUnfinishedUpload(t *testing.T) {
	const boundary = "test-boundary"
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"oversized spec", strings.Repeat("x", 2*maxSpecBytes), http.StatusBadRequest},
		{"saturated", "{\"taskId\":\"second\"}\r\n--" + boundary + "\r\n", http.StatusTooManyRequests},
		{"local client", "{\"taskId\":\"local\"}\r\n--" + boundary + "\r\n", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New(Config{MaxConcurrency: 1})
			if tc.status == http.StatusTooManyRequests {
				d.lanes <- struct{}{}
			}
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, mr := d.admit(w, r); mr != nil {
					d.releaseLane()
					w.WriteHeader(http.StatusOK)
				}
			})
			if tc.status == http.StatusForbidden {
				// The test client connects over loopback, so the source guard rejects it.
				handler = d.Handler()
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			body := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"spec\"\r\n\r\n" + tc.body
			// Leave the chunked stream open: the server must not wait for more bytes.
			if _, err := fmt.Fprintf(conn, "POST /tasks HTTP/1.1\r\nHost: localhost\r\nContent-Type: multipart/form-data; boundary=%s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", boundary, len(body), body); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status || !resp.Close {
				t.Fatalf("status = %d, close = %v; want %d and close", resp.StatusCode, resp.Close, tc.status)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatalf("waiting for connection to close: got %v, want EOF", err)
			}
		})
	}
}

type failRead struct{ t *testing.T }

func (r failRead) Read([]byte) (int, error) {
	r.t.Fatal("read past spec")
	return 0, io.EOF
}
