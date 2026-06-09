package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const maxFirecrackerAPIResponseBodyBytes int64 = 64 << 10

// firecrackerAPIClient talks to one Firecracker VMM API socket.
type firecrackerAPIClient struct {
	client *http.Client
}

func newFirecrackerAPIClient(socketPath string) *firecrackerAPIClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &firecrackerAPIClient{client: &http.Client{Transport: transport}}
}

func (c *firecrackerAPIClient) putJSON(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxFirecrackerAPIResponseBodyBytes))
		return fmt.Errorf("firecracker API PUT %s returned status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxFirecrackerAPIResponseBodyBytes)); err != nil {
		return fmt.Errorf("firecracker API PUT %s drain response body: %w", path, err)
	}
	return nil
}
