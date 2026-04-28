package manager

// ResourceLimits holds the Docker resource limits for a sandbox.
type ResourceLimits struct {
	// MemoryMB is the hard memory limit in megabytes. 0 means use default.
	MemoryMB int64 `json:"memory_mb,omitempty"`
	// CPUPercent is the CPU limit as a percentage of one core.
	CPUPercent int `json:"cpu_percent,omitempty"`
	// PidsMax is the maximum number of processes. 0 means use default.
	PidsMax int `json:"pids_max,omitempty"`
}
