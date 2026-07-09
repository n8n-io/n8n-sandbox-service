# Firecracker golden build

This bundle ships the scripts used to build the Firecracker rootfs template and
golden snapshot on a sandbox runner VM. It is published as a GitHub Release
asset alongside each service release (`service/v{version}`).

## Contents (schema v2)

| Path | Purpose |
|------|---------|
| `MANIFEST.json` | Service version, git ref, entrypoints, and pinned tool versions |
| `scripts/build-rootfs-template.sh` | Build `rootfs.ext4` and install `vmlinux` from Firecracker CI assets |
| `scripts/configure-host-nat.sh` | Host IPv4 forwarding and NAT/FORWARD rules for sandbox netns egress |
| `scripts/create-golden-snapshot.sh` | Build the golden snapshot from kernel, rootfs, and daemon |
| `scripts/setup-firecracker-e2e-vm.sh` | Full e2e VM bootstrap (delegates to the scripts above) |
| `bin/sandbox-daemon` | Pre-built linux/amd64 sandbox daemon at package time |

## Usage on a runner VM

1. Download the tarball for the service version you deploy:

   ```bash
   VERSION=1.1.0
   curl -fsSL -o firecracker-golden-build.tar.gz \
     "https://github.com/n8n-io/n8n-sandbox-service/releases/download/service/v${VERSION}/firecracker-golden-build-${VERSION}.tar.gz"
   tar xzf firecracker-golden-build.tar.gz
   cd firecracker-golden-build
   ```

2. Configure host NAT (once per boot or before snapshot creation):

   ```bash
   sudo ./scripts/configure-host-nat.sh
   ```

3. Build the rootfs template from baked or downloaded Firecracker CI assets:

   ```bash
   sudo env \
     FIRECRACKER_CI_VMLINUX=/path/to/vmlinux \
     FIRECRACKER_CI_ROOTFS_SQUASHFS=/path/to/ubuntu.squashfs \
     TEMPLATE_DIR=/srv/firecracker/template \
     ./scripts/build-rootfs-template.sh
   ```

4. Create the golden snapshot:

   ```bash
   sudo ./scripts/create-golden-snapshot.sh \
     --kernel /srv/firecracker/template/vmlinux \
     --ext4 /srv/firecracker/template/rootfs.ext4 \
     --daemon-bin ./bin/sandbox-daemon \
     --out /srv/firecracker/snapshots
   ```

For full e2e VM bootstrap (installs host deps, downloads CI assets from S3,
builds daemon from source), use `setup-firecracker-e2e-vm.sh` from a checkout of
this repository.

See the Firecracker runner README in this repository at
`internal/runner/runtime/firecracker.ee/README.md` for runtime configuration on
the runner host.
