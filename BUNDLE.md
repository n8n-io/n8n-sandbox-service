# Firecracker golden-build bundle contract

The sandbox service publishes `firecracker-golden-build-<version>.tar.gz` on each
`service/v*` GitHub Release (and staging prereleases). This document is the
source of truth for what the tarball contains and how consumers should use it.

Infra-specific wiring (Azure VMSS, Key Vault, gallery images) lives in
[n8n-cloud-infrastructure-next](https://github.com/n8n-io/n8n-cloud-infrastructure-next)
under `vm-images/firecracker-sandbox-runner/`.

## Container images vs tarball

| Artifact | Registry | When | Images |
| --- | --- | --- | --- |
| Versioned service release | [Docker Hub](https://hub.docker.com/u/n8nio) | Merge `service/release/*` PR | `n8nio/n8n-sandbox-service-api`, `n8nio/n8n-sandbox-service-runner-dind`, `n8nio/n8n-sandbox-service-runner-firecracker` (amd64) |
| Versioned sandbox release | Docker Hub | Merge `sandbox/release/*` PR | `n8nio/n8n-sandbox-service-sandbox` |
| Alpha (every push to `main`) | Private ACR | `release-alpha` workflow | `api`, `runner-dind`, `runner-firecracker`, `sandbox` (`:alpha`, `:<full_sha>`) |
| Staging candidates | Private ACR | Publish Service Staging workflow | Same four images (`:<version>-staging.<sha>`, `:<full_sha>`) |
| Golden build scripts | GitHub Release asset | Service release / staging | `firecracker-golden-build-{version}.tar.gz` (`bin/sandbox-daemon` + host/snapshot scripts) |

Public adopters pull API, runner-dind, runner-firecracker, and sandbox from Docker Hub on
versioned releases. Alpha/staging Firecracker images remain on the private registry until
the next service release; pin the golden-build tarball `git_sha` to the image tag SHA.

Pin everything to the same commit: compare `MANIFEST.json` `git_sha` with the
container image's full-SHA tag.

## Ownership split

This repository owns (ship in the tarball and/or release docs):

- Generic runner host install (`install-runner-host.sh`)
- Firecracker CI asset download (`firecracker-ci-assets.sh`)
- Rootfs template build (`build-rootfs-template.sh`)
- Golden snapshot creation (`create-golden-snapshot.sh`)
- Host NAT / forwarding (`configure-host-nat.sh`)
- Pre-built `bin/sandbox-daemon` at package time
- `MANIFEST.json` with entrypoints, versions, and checksums
- E2e full bootstrap (`setup-firecracker-e2e-vm.sh`, shipped for reference)

Infra repo owns (not in the bundle):

- Azure Compute Gallery image build and publish
- Cloud-init, Key Vault, TLS, systemd units
- Baked Firecracker CI assets at gallery publish time (avoid S3 on first boot)
- Runner subnet / NAT Gateway / NSG / NIC IP forwarding
- Pulling `runner-firecracker` from ACR for n8n staging (or building gallery images)

## Bundle layout (schema v2)

```text
firecracker-golden-build/
  MANIFEST.json
  README.md
  scripts/
    install-runner-host.sh
    firecracker-ci-assets.sh
    build-rootfs-template.sh
    configure-host-nat.sh
    create-golden-snapshot.sh
    setup-firecracker-e2e-vm.sh
  bin/
    sandbox-daemon
```

### Entrypoints (`MANIFEST.json`)

| Key | Script | Purpose |
| --- | --- | --- |
| `install_runner_host` | `scripts/install-runner-host.sh` | apt packages, Firecracker/jailer, dirs, sysctl, NAT |
| `firecracker_ci_assets` | `scripts/firecracker-ci-assets.sh` | Download/verify CI kernel + squashfs |
| `build_rootfs_template` | `scripts/build-rootfs-template.sh` | Build `rootfs.ext4` + install `vmlinux` |
| `create_snapshot` | `scripts/create-golden-snapshot.sh` | Host-local golden snapshot |
| `configure_host_nat` | `scripts/configure-host-nat.sh` | iptables MASQUERADE + FORWARD for `fc-veth+` |

All scripts in the tarball are packaged with mode `0755`.

### `install-runner-host.sh`

Runs as root. Installs host packages, Firecracker/jailer, runtime directories,
persistent `net.ipv4.ip_forward`, and delegates to `configure-host-nat.sh`.

Options: `--skip-packages`, `--skip-firecracker`, `--download-ci-assets`.

Out of scope: crane, registry pulls, systemd, Key Vault, golden-build install,
baked `runner-firecracker` binary.

### `build-rootfs-template.sh`

Inputs (flags or env):

- `FIRECRACKER_CI_VMLINUX`, `FIRECRACKER_CI_ROOTFS_SQUASHFS`
- `TEMPLATE_DIR` (writes `rootfs.ext4`, installs `vmlinux`)
- `FIRECRACKER_ROOTFS_SIZE_MB` (default `1024`)

Must seed `/etc/resolv.conf` (remove squashfs symlink first; write nameservers).

### `configure-host-nat.sh`

Idempotent shell equivalent of `EnsureHostNAT` in
`internal/runner/runtime/firecracker.ee/network/network.go`:

- `net.ipv4.ip_forward=1`
- `MASQUERADE` on the default-route interface
- `FORWARD` accept for `fc-veth+` and `ESTABLISHED,RELATED`

### `bin/sandbox-daemon`

linux/amd64 binary built at package time. `MANIFEST.json` includes `sha256` for
verification. Infra may bake this onto gallery images instead of pulling a
separate container image.

## Consumer workflow

See [docs/quickstart-firecracker-linux.md](docs/quickstart-firecracker-linux.md) for a
step-by-step host setup. Tarball `README.md` lists the same entrypoints for operators
on a runner VM.

## Packaging and CI

Package locally:

```sh
./scripts/package-firecracker-golden-build.sh --version "$(tr -d '[:space:]' < SERVICE_VERSION)"
```

CI runs `scripts/test-firecracker-golden-build-bundle.sh` (rootfs build, resolv.conf
check, tarball layout, executable entrypoints). Release workflows attach the tarball
to `service/v*` GitHub Releases.

## Copy-on-release rule

Deploy golden-build scripts only from the tarball for the exact service version
you ship. Do not fork rootfs/NAT/snapshot logic in consumer repos — call bundle
entrypoints or fail loudly when they are missing.

Rollout order on Firecracker runners:

1. Install/replace bundle on the host (or bake into a new gallery image).
2. Rebuild the host-local snapshot using bundle entrypoints.
3. Roll `runner-firecracker` to the matching commit/version.
4. Gate on smoke tests.

## Cloud-specific notes (Azure)

Sandbox netns egress is forwarded traffic (`fc-veth*` → default NIC). Linux
`ip_forward` and iptables alone are not enough on Azure — the VM NIC needs
`enable_ip_forwarding = true`. Host-originated `curl` can work while guest egress
fails without it.

See infra `terraform/.../sandbox-firecracker.tf` and
`charts/firecracker-sandbox-service/README.md` (network topology).
