package volume

// MaxDrives is the most volume drives a Firecracker sandbox can have:
// Firecracker's x86_64 limit of 17 MMIO devices, less the rootfs and the NIC.
const MaxDrives = 15
