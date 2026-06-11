package firecracker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	reader *strings.Reader
	closed bool
	eof    bool
}

func newTrackingReadCloser(s string) *trackingReadCloser {
	return &trackingReadCloser{reader: strings.NewReader(s)}
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.eof = true
	}
	return n, err
}

func (b *trackingReadCloser) Close() error {
	b.closed = true
	return nil
}

func TestFirecrackerAPIClientPutJSONDrainsSuccessfulResponseBody(t *testing.T) {
	body := newTrackingReadCloser("ok")
	client := &firecrackerAPIClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", req.Method)
		}
		if req.URL.Path != "/snapshot/load" {
			t.Fatalf("path = %s, want /snapshot/load", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})}}

	if err := client.putJSON(context.Background(), "/snapshot/load", []byte(`{}`)); err != nil {
		t.Fatalf("putJSON() error = %v", err)
	}
	if !body.eof {
		t.Fatal("expected successful response body to be drained to EOF")
	}
	if !body.closed {
		t.Fatal("expected successful response body to be closed")
	}
}

func TestFirecrackerAPIClientPutJSONLimitsErrorResponseBody(t *testing.T) {
	body := strings.Repeat("x", int(maxFirecrackerAPIResponseBodyBytes)+1)
	client := &firecrackerAPIClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}}

	err := client.putJSON(context.Background(), "/snapshot/load", []byte(`{}`))
	if err == nil {
		t.Fatal("expected putJSON() error")
	}
	if len(err.Error()) > int(maxFirecrackerAPIResponseBodyBytes)+256 {
		t.Fatalf("error body was not bounded, got error length %d", len(err.Error()))
	}
}
