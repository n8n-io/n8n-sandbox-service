# Firecracker golden-build bundle contract

The sandbox service publishes `firecracker-golden-build-<version>.tar.gz` on each
`service/v*` GitHub Release (and staging prereleases). This document is the
source of truth for what the tarball contains and how consumers should use it.

## Container images vs tarball

| Artifact | Registry | When | Images |
| --- | --- | --- | --- |
| Versioned release | [Docker Hub](https://hub.docker.com/u/n8nio) | Merge `service/release/*` PR | `n8nio/n8n-sandbox-service-api`, `n8nio/n8n-sandbox-service-runner-dind`, `n8nio/n8n-sandbox-service-runner-firecracker` (amd64), `n8nio/n8n-sandbox-service-sandbox` — all at the same version |
| Alpha (every push to `main`) | Private ACR | `release-alpha` workflow | `api`, `runner-dind`, `runner-firecracker`, `sandbox` (`:alpha`, `:<full_sha>`) |
| Staging candidates | Private ACR | Publish Service Staging workflow | Same four images (`:<version>-staging.<sha>`, `:<full_sha>`) |
| Golden build scripts | GitHub Release asset | Service release / staging | `firecracker-golden-build-{version}.tar.gz` (`bin/sandbox-daemon` + host/snapshot scripts) |

Public adopters pull API, runner-dind, runner-firecracker, and sandbox from Docker Hub on
versioned releases. Alpha/staging Firecracker images remain on the private registry until
the next service release; pin the golden-build tarball `git_sha` to the image tag SHA.

Pin everything to the same commit: compare `MANIFEST.json` `git_sha` with the
commit the images were built from — the `service/v{version}` tag for a release,
the image's full-SHA tag for alpha and staging.

## Scope

The bundle ships (sources under `scripts/firecracker.ee/`):

- Generic runner host install (`install-runner-host.sh`)
- Firecracker CI kernel download (`firecracker-ci-assets.sh`)
- Rootfs template build from sandbox OCI image (`build-rootfs-template.sh`)
- Golden snapshot creation (`create-golden-snapshot.sh`)
- Host NAT / forwarding (`configure-host-nat.sh`)
- Pre-built `bin/sandbox-daemon` at package time
- `MANIFEST.json` with entrypoints, sandbox image pin, versions, and checksums
- E2e full bootstrap (`setup-firecracker-e2e-vm.sh`, shipped for reference)

Not in the bundle: VM image builds, secret and TLS material, systemd units, and
cloud network setup. Those are the operator's.

## Bundle layout (schema v3)

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

### Versions (`MANIFEST.json`)

| Key | Meaning |
| --- | --- |
| `version` | Release version of the tree the bundle was built from (the `VERSION` file). On a service release this is the tag all four images carry. |
| `bundle_version` | Label of this tarball. Equals `version` on a service release; on a staging prerelease it is the staging label (`{version}-staging.{sha}`). |
| `git_sha` | Commit the bundle was built from. This, not `version`, is the pin to correlate with the commit the container images were built from. |
| `sandbox_image.ref` | Authoritative pin for the guest rootfs. Staging pins the ACR commit-SHA tag, so it does not carry the release version. |

Staging bundles are the case to be careful with: nothing is published at `version`
during a staging run, so use `bundle_version`, `git_sha`, and `sandbox_image.ref`
to identify what that candidate actually contains.

### Entrypoints (`MANIFEST.json`)

| Key | Script | Purpose |
| --- | --- | --- |
| `install_runner_host` | `scripts/install-runner-host.sh` | apt packages, Firecracker/jailer, dirs, sysctl, NAT |
| `firecracker_ci_assets` | `scripts/firecracker-ci-assets.sh` | Download/verify CI `vmlinux` |
| `build_rootfs_template` | `scripts/build-rootfs-template.sh` | Build `rootfs.ext4` from sandbox image + install `vmlinux` |
| `create_snapshot` | `scripts/create-golden-snapshot.sh` | Host-local golden snapshot |
| `configure_host_nat` | `scripts/configure-host-nat.sh` | iptables MASQUERADE + FORWARD for `fc-veth+` |

All scripts in the tarball are packaged with mode `0755`.

### `install-runner-host.sh`

Runs as root. Installs host packages, Firecracker/jailer, runtime directories,
persistent `net.ipv4.ip_forward`, and delegates to `configure-host-nat.sh`.

Options: `--skip-packages`, `--skip-firecracker`, `--download-ci-assets`.

Out of scope: registry pulls, systemd units, secrets, installing the bundle
itself, the `runner-firecracker` binary.

### `build-rootfs-template.sh`

Inputs (flags or env):

- `FIRECRACKER_CI_VMLINUX` (or download via `firecracker-ci-assets.sh`)
- Exactly one of `SANDBOX_IMAGE` (OCI ref; needs crane or docker) or `SANDBOX_ROOTFS_TAR`
- `TEMPLATE_DIR` (writes `rootfs.ext4`, installs `vmlinux`)
- `FIRECRACKER_ROOTFS_SIZE_MB` (default `2048`; must leave ≥128MiB free after ext4 metadata for guest writes)

Must seed `/etc/resolv.conf` (remove image symlink first; write `8.8.8.8` / `1.1.1.1`).

Guest userspace comes from the sandbox image pinned in `MANIFEST.json`
(`sandbox_image.ref`, from `VERSION`). Snapshot create still injects the
service bundle's `bin/sandbox-daemon` as `/sandbox-daemon` (PID 1). After the
staged tree is normalized to `root:root`, `/home/user` is restored to `1000:1000`
so the daemon (which drops privileges) can write the workspace.

### `configure-host-nat.sh`

Idempotent shell equivalent of `EnsureHostNAT` in
`internal/runner/runtime/firecracker.ee/network/network.go`:

- `net.ipv4.ip_forward=1`
- `MASQUERADE` on the default-route interface
- `FORWARD` accept for `fc-veth+` and `ESTABLISHED,RELATED`

On cloud VMs the NIC may also need IP forwarding enabled at the cloud level;
without it host-originated traffic works while guest egress fails.

### `bin/sandbox-daemon`

linux/amd64 binary built at package time. `MANIFEST.json` includes `sha256` for
verification.

## Consumer workflow

Step-by-step host setup: [docs/quickstart-firecracker-linux.md](docs/quickstart-firecracker-linux.md). The tarball's `README.md` ([source](scripts/firecracker-golden-build/README.md)) carries the same commands for operators without a checkout.

## Packaging and CI

Package locally:

```sh
./scripts/package-firecracker-golden-build.sh --version "$(tr -d '[:space:]' < VERSION)"
```

`sandbox_image.ref` defaults to `n8nio/n8n-sandbox-service-sandbox:{VERSION}`. Set `SANDBOX_IMAGE_REF` to pin elsewhere — a digest ref for a reproducible bundle, or another registry (the staging workflow pins its candidate this way); `repository` and `tag` in the manifest derive from it, with `tag` empty for a digest ref.

CI runs `scripts/test-firecracker-golden-build-bundle.sh` (rootfs build, resolv.conf
check, tarball layout, executable entrypoints). Release workflows attach the tarball
to `service/v*` GitHub Releases.

## Copy-on-release rule

Deploy golden-build scripts only from the tarball for the exact service version
you ship. Do not fork rootfs/NAT/snapshot logic in consumer repos — call bundle
entrypoints or fail loudly when they are missing.

Rollout order per environment:

1. Install/replace the bundle on each runner host. Assert `git_sha` in
   `MANIFEST.json` matches the commit the runner image was built from (see
   above).
2. Ensure the rootfs template exists (`build_rootfs_template`; rebuild it when
   `sandbox_image.ref` changed).
3. Set `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` (and
   `SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN`) so the runner creates the host-local
   golden snapshot on first `Prepare`, or run `create-golden-snapshot.sh` by hand.
4. Roll `runner-firecracker` to the matching version — after step 3, never before.
   The runner stays unhealthy (`/readyz`, registration `Healthy=false`) until pin,
   snapshot and canary pass.
5. Roll API, dind and sandbox images to the same version.
6. Gate on `scripts/smoke-sandbox.sh` against the deployed API.
