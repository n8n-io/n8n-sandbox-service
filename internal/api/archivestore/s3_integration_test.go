//go:build integration

package archivestore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Against a real S3-compatible store (MinIO in CI or locally):
// SANDBOX_TEST_S3_ENDPOINT, _ACCESS_KEY, _SECRET_KEY.
func TestS3PresignedRoundTrip(t *testing.T) {
	endpoint := os.Getenv("SANDBOX_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SANDBOX_TEST_S3_ENDPOINT not set")
	}
	s, err := NewS3(endpoint, "archives-test", "us-east-1", os.Getenv("SANDBOX_TEST_S3_ACCESS_KEY"), os.Getenv("SANDBOX_TEST_S3_SECRET_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := http.NewRequest(http.MethodPut, s.presignPath(http.MethodPut, "/archives-test", time.Minute, false), nil)
	if resp, err := http.DefaultClient.Do(mk); err == nil {
		resp.Body.Close()
	}
	const obj = "tenant-1/sb-1/a1"
	up, _ := s.UploadURL(obj)
	req, _ := http.NewRequest(http.MethodPut, up, strings.NewReader("sealed archive"))
	req.Header.Set("If-None-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %v %v", resp, err)
	}
	// The upload URL writes once.
	req, _ = http.NewRequest(http.MethodPut, up, strings.NewReader("garbage"))
	req.Header.Set("If-None-Match", "*")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second put: %v %v", resp, err)
	}
	if n, err := s.Size(context.Background(), obj); err != nil || n != 14 {
		t.Fatalf("size %d %v", n, err)
	}
	down, _ := s.DownloadURL(obj)
	resp, err = http.Get(down)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "sealed archive" {
		t.Fatalf("get %q", b)
	}
	// The download URL does not write.
	req, _ = http.NewRequest(http.MethodPut, down, strings.NewReader("x"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("put with get url: %v %v", resp, err)
	}
	if err := s.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Size(context.Background(), obj); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
