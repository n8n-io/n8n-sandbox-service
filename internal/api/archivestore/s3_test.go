package archivestore

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The example in AWS's "Authenticating Requests: Using Query Parameters"
// (Signature Version 4), with its published signature.
func TestPresignMatchesAWSExample(t *testing.T) {
	s, err := NewS3("https://examplebucket.s3.amazonaws.com", "examplebucket", "us-east-1",
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	got := s.presignPath(http.MethodGet, "/test.txt", 86400*time.Second, false)
	const want = "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.Contains(got, want) || !strings.HasPrefix(got, "https://examplebucket.s3.amazonaws.com/test.txt?") {
		t.Fatalf("presigned URL %s\nwant signature %s", got, want)
	}
}

func TestS3RefusesBadObjects(t *testing.T) {
	s, _ := NewS3("https://s3.example.com", "b", "r", "a", "k")
	if _, err := s.UploadURL("../x"); err == nil {
		t.Fatal("presigned an invalid object")
	}
	if _, err := NewS3("ftp://x", "b", "r", "a", "k"); err == nil {
		t.Fatal("accepted a non-http endpoint")
	}
}
