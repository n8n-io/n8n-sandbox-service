# Firecracker golden build

Scripts to build the Firecracker rootfs template and golden snapshot on a sandbox runner host. Published as a GitHub Release asset with each service release (`service/v{version}`); the tarball contract is `BUNDLE.md` in the repository.

## Contents (schema v3)

| Path | Purpose |
|------|---------|
| `MANIFEST.json` | Release version, `git_sha`, sandbox image pin, entrypoints, checksums |
| `scripts/install-runner-host.sh` | Host packages, Firecracker/jailer, dirs, NAT |
| `scripts/firecracker-ci-assets.sh` | Download/verify Firecracker CI `vmlinux` |
| `scripts/build-rootfs-template.sh` | Build `rootfs.ext4` from the sandbox OCI image + install `vmlinux` |
| `scripts/configure-host-nat.sh` | IPv4 forwarding and MASQUERADE/FORWARD rules for sandbox egress |
| `scripts/create-golden-snapshot.sh` | Build the golden snapshot (`snapshot_mem`, `snapshot_state`, `boot.json`) |
| `scripts/setup-firecracker-e2e-vm.sh` | Full e2e VM bootstrap (for reference) |
| `bin/sandbox-daemon` | Pre-built linux/amd64 sandbox daemon |

## Usage on a runner host

Run from the extracted tarball root. `MANIFEST.json` records the version and `git_sha` this bundle was built from; the runner image must be built from the same commit.

```bash
# 1. Host prerequisites. Omit --download-ci-assets when vmlinux is already under /srv/firecracker/ci-assets.
sudo ./scripts/install-runner-host.sh --download-ci-assets

# 2. Rootfs template. Needs crane or docker on PATH to unpack SANDBOX_IMAGE
#    (or pass SANDBOX_ROOTFS_TAR to a pre-exported tar and skip both).
source /srv/firecracker/ci-assets/manifest.env
SANDBOX_IMAGE="$(jq -r .sandbox_image.ref MANIFEST.json)"
sudo env \
  FIRECRACKER_CI_VMLINUX="$FIRECRACKER_CI_VMLINUX" \
  SANDBOX_IMAGE="$SANDBOX_IMAGE" \
  FIRECRACKER_ROOTFS_SIZE_MB="$(jq -r .firecracker_rootfs_size_mb MANIFEST.json)" \
  TEMPLATE_DIR=/srv/firecracker/template \
  ./scripts/build-rootfs-template.sh

# 3. Golden snapshot (injects bin/sandbox-daemon as PID 1). Not portable: run on every host.
sudo ./scripts/create-golden-snapshot.sh \
  --kernel /srv/firecracker/template/vmlinux \
  --ext4 /srv/firecracker/template/rootfs.ext4 \
  --daemon-bin ./bin/sandbox-daemon \
  --out /srv/firecracker/snapshots

# 4. The runner's default snapshot paths are .../snapshots/mem and .../snapshots/state.
sudo ln -sf snapshot_mem /srv/firecracker/snapshots/mem
sudo ln -sf snapshot_state /srv/firecracker/snapshots/state
```

`snapshot_mem`, `snapshot_state` and `boot.json` describe one build; keep them together and rebuild the set rather than editing `boot.json`. Alternatively point the runner at this bundle via `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` and `SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN` and it creates the snapshot itself on first start.

Runner installation and configuration: <https://github.com/n8n-io/n8n-sandbox-service/blob/main/docs/quickstart-firecracker-linux.md>.
