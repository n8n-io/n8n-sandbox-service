package archivestore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDirSignedURLs(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	d, err := NewDir(t.TempDir(), srv.URL, []byte("signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = d
	const obj = "tenant-1/sb-1/a1"

	up, _ := d.UploadURL(obj)
	req, _ := http.NewRequest(http.MethodPut, up, strings.NewReader("sealed"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %v %v", resp, err)
	}
	// The upload URL writes once.
	req, _ = http.NewRequest(http.MethodPut, up, strings.NewReader("garbage"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second put: %v %v", resp, err)
	}
	if n, err := d.Size(context.Background(), obj); err != nil || n != 6 {
		t.Fatalf("size %d %v", n, err)
	}
	down, _ := d.DownloadURL(obj)
	resp, err := http.Get(down)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "sealed" {
		t.Fatalf("got %q", b)
	}

	// The upload URL does not read, the download URL does not write, a URL for
	// one object does not open another, and an expired or forged one opens nothing.
	for name, tc := range map[string]struct{ method, url string }{
		"get with upload url": {http.MethodGet, up},
		"put with download":   {http.MethodPut, down},
		"other object":        {http.MethodGet, strings.Replace(down, "/a1?", "/a2?", 1)},
		"forged signature":    {http.MethodGet, down[:len(down)-4] + "0000"},
		"no signature":        {http.MethodGet, srv.URL + HandlerPrefix + obj},
		"traversal":           {http.MethodGet, srv.URL + HandlerPrefix + "../x/y?exp=9999999999&sig=0"},
	} {
		req, _ := http.NewRequest(tc.method, tc.url, strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	d.now = func() time.Time { return time.Now().Add(2 * urlLifetime) }
	if resp, _ := http.Get(down); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired url: %d", resp.StatusCode)
	}

	if err := d.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Size(context.Background(), obj); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestValidObject(t *testing.T) {
	for obj, want := range map[string]bool{
		"t/s/a": true, "t/s": false, "t/s/a/b": false, "../s/a": false, "t/./a": false, "t/s/a.b": false, "": false,
	} {
		if ValidObject(obj) != want {
			t.Errorf("%q", obj)
		}
	}
}

// An upload slower than the serving API's read timeout still lands: its
// signed URL's expiry bounds it instead.
func TestDirUploadOutlastsTheServerReadTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()
	d, err := NewDir(t.TempDir(), srv.URL, []byte("signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = d
	const obj = "tenant-1/sb-1/slow"
	up, _ := d.UploadURL(obj)

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(150 * time.Millisecond)
			_, _ = pw.Write([]byte("chunk"))
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPut, up, pr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("slow put: %v %v", resp, err)
	}
	if n, err := d.Size(context.Background(), obj); err != nil || n != 25 {
		t.Fatalf("size %d %v", n, err)
	}
}
