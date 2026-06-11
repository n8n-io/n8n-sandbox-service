package firecracker

import (
	"context"
	"encoding/json"
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
func loadSnapshot(ctx context.Context, socketPath string, _ Config) error {
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

	client := newFirecrackerAPIClient(socketPath)
	return client.putJSON(ctx, "/snapshot/load", body)
}
