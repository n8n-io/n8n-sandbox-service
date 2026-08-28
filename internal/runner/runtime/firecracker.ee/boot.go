package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
)

type machineConfigRequest struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type bootSourceRequest struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type driveRequest struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterfaceRequest struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type actionRequest struct {
	ActionType string `json:"action_type"`
}

// coldBoot builds a microVM from scratch on the sandbox's existing rootfs, for a
// sandbox whose snapshot can no longer be restored. It replays what
// create-golden-snapshot.sh booted the golden snapshot with, taken from that
// snapshot's own sidecar rather than from current configuration, so a recovered
// sandbox comes up as the guest it was created as: same vCPUs, same memory, same
// kernel command line, same init.
//
// Nothing here is shared with the script that first booted these values, because
// they cannot be: the script runs standalone on runner VMs, before the release
// that carries this binary. The sidecar is the contract between them, which is why
// these requests are a transcription of the script's rather than a generalisation
// of them.
func coldBoot(ctx context.Context, socketPath string, params *bootParams) error {
	if params == nil {
		return fmt.Errorf("cold boot needs the boot parameters of the snapshot the sandbox was created from, and this sandbox has none")
	}

	client := newFirecrackerAPIClient(socketPath)
	put := func(path string, body any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode firecracker %s request: %w", path, err)
		}
		if err := client.putJSON(ctx, path, raw); err != nil {
			return fmt.Errorf("firecracker %s: %w", path, err)
		}
		return nil
	}

	if err := put("/machine-config", machineConfigRequest{
		VCPUCount:  params.VCPUCount,
		MemSizeMiB: params.MemSizeMiB,
		SMT:        false,
	}); err != nil {
		return err
	}
	if err := put("/boot-source", bootSourceRequest{
		KernelImagePath: params.KernelImagePath,
		BootArgs:        params.BootArgs,
	}); err != nil {
		return err
	}
	// Writable, and the root device: this is the sandbox's own rootfs clone, the
	// filesystem a cold boot exists to preserve.
	if err := put("/drives/rootfs", driveRequest{
		DriveID:      "rootfs",
		PathOnHost:   params.RootfsDrivePath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return err
	}
	// The MAC is replayed too. The guest configured its network from the kernel
	// ip= parameter at first boot and that configuration is in the rootfs, so a
	// different MAC would boot a guest whose interface it does not recognise.
	if err := put("/network-interfaces/eth0", networkInterfaceRequest{
		IfaceID:     "eth0",
		GuestMAC:    params.GuestMAC,
		HostDevName: params.HostTapDeviceName,
	}); err != nil {
		return err
	}
	// Last, because everything above only configures the VMM. This is the call
	// that starts executing the kernel.
	return put("/actions", actionRequest{ActionType: "InstanceStart"})
}
