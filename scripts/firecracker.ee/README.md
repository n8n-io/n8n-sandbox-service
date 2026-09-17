# Firecracker golden-build scripts (enterprise)

Canonical source for scripts packaged into
`firecracker-golden-build-<version>.tar.gz` and run on runner hosts. The `.ee` directory name marks them as Firecracker enterprise-licensed
code (see `AGENTS.md`).

| Script | Role |
|--------|------|
| `install-runner-host.sh` | Host packages, Firecracker/jailer, dirs, NAT |
| `firecracker-ci-assets.sh` | Download the pinned CI `vmlinux`, verify its SHA-256 |
| `bump-firecracker-kernel.sh` | Move that pin to the newest same-line kernel (weekly workflow, not in the bundle) |
| `build-rootfs-template.sh` | `rootfs.ext4` from sandbox OCI image + `vmlinux` |
| `configure-host-nat.sh` | Host MASQUERADE / FORWARD for `fc-veth+` |
| `create-golden-snapshot.sh` | Host-local golden snapshot (injects daemon as PID 1) |
| `setup-firecracker-e2e-vm.sh` | Full e2e VM bootstrap (uses the scripts above) |

Packaging: `scripts/package-firecracker-golden-build.sh` copies these into the
release tarball under `scripts/`. Contract: [BUNDLE.md](../../BUNDLE.md).

Azure VM provision/cleanup for e2e stays under `e2e/infra/scripts/`.
