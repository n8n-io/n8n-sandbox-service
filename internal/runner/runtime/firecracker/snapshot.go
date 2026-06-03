package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

type loadSnapshotRequest struct {
	SnapshotPath    string     `json:"snapshot_path"`
	MemBackend      memBackend `json:"mem_backend"`
	TrackDirtyPages bool       `json:"track_dirty_pages"`
	ResumeVM        bool       `json:"resume_vm"`
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// loadSnapshot asks the Firecracker API socket to restore the configured full
// snapshot and immediately resume the VM.
func loadSnapshot(ctx context.Context, socketPath string, _ config.FirecrackerConfig) error {
	body, err := json.Marshal(loadSnapshotRequest{
		SnapshotPath: "/snapshot_state",
		MemBackend: memBackend{
			BackendType: "File",
			BackendPath: "/snapshot_mem",
		},
		TrackDirtyPages: false,
		ResumeVM:        true,
	})
	if err != nil {
		return err
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker/snapshot/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker snapshot load returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
