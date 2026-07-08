# Firecracker golden build

This bundle ships the scripts used to build the Firecracker rootfs template and
golden snapshot on a sandbox runner VM. It is published as a GitHub Release
asset alongside each service release (`service/v{version}`).

## Contents

| Path | Purpose |
|------|---------|
| `MANIFEST.json` | Service version, git ref, and pinned tool versions |
| `scripts/create-golden-snapshot.sh` | Build the golden snapshot from kernel, rootfs, and daemon |
| `scripts/setup-firecracker-e2e-vm.sh` | Full VM bootstrap (kernel, rootfs template, snapshot) |

## Usage on a runner VM

1. Download the tarball for the service version you deploy:

   ```bash
   VERSION=1.1.0
   curl -fsSL -o firecracker-golden-build.tar.gz \
     "https://github.com/n8n-io/n8n-sandbox-service/releases/download/service/v${VERSION}/firecracker-golden-build-${VERSION}.tar.gz"
   tar xzf firecracker-golden-build.tar.gz
   cd firecracker-golden-build
   ```

2. Run the setup script from a checkout of this repository (it builds the
   runner binary and installs host dependencies):

   ```bash
   sudo ./scripts/setup-firecracker-e2e-vm.sh
   ```

   Or run only snapshot creation when kernel, rootfs template, and daemon
   binary are already present:

   ```bash
   sudo ./scripts/create-golden-snapshot.sh \
     --kernel /srv/firecracker/kernel/vmlinux \
     --ext4 /srv/firecracker/template/rootfs.ext4 \
     --daemon-bin /path/to/daemon \
     --out /srv/firecracker/snapshots
   ```

See the Firecracker runner README in this repository at
`internal/runner/runtime/firecracker.ee/README.md` for runtime configuration on
the runner host.
