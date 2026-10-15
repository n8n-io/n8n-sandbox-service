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
	ClockRealtime   bool       `json:"clock_realtime"`
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type createSnapshotRequest struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// loadSnapshot asks the Firecracker API socket to restore a full snapshot and
// immediately resume the VM.
//
// clock_realtime advances the guest's kvm-clock by the wall-clock time since the
// snapshot was taken. Without it the guest resumes with the clock it had then: a
// new sandbox runs behind by the age of the golden snapshot, a woken one by how
// long it was stopped. The guest's monotonic clock jumps by the same amount.
// Firecracker supports it on x86_64 only, and refuses the restore unless KVM
// recorded the wall-clock time in the snapshot (host kernel 5.16 or later with a
// TSC-based clocksource).
func loadSnapshot(ctx context.Context, socketPath string, _ Config) error {
	body, err := json.Marshal(loadSnapshotRequest{
		SnapshotPath: "/snapshot_state",
		MemBackend: memBackend{
			BackendType: "File",
			BackendPath: "/snapshot_mem",
		},
		TrackDirtyPages: false,
		ResumeVM:        true,
		ClockRealtime:   true,
	})
	if err != nil {
		return err
	}

	client := newFirecrackerAPIClient(socketPath)
	return client.putJSON(ctx, "/snapshot/load", body)
}

// pauseVM pauses a running microVM before snapshot/create.
func pauseVM(ctx context.Context, socketPath string) error {
	client := newFirecrackerAPIClient(socketPath)
	return client.patchJSON(ctx, "/vm", []byte(`{"state":"Paused"}`))
}

// createSnapshot writes a full VM snapshot to the jail bind mounts.
func createSnapshot(ctx context.Context, socketPath string) error {
	body, err := json.Marshal(createSnapshotRequest{
		SnapshotType: "Full",
		SnapshotPath: "/snapshot_state",
		MemFilePath:  "/snapshot_mem",
	})
	if err != nil {
		return err
	}
	client := newFirecrackerAPIClient(socketPath)
	return client.putJSON(ctx, "/snapshot/create", body)
}
