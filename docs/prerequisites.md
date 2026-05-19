# Prerequisites

## Required

- **Docker** >= 24.x — the sandbox service runs as Docker containers and runners manage Docker-in-Docker sandbox lifecycles.

## Linux only

- **[sysbox-runc](https://github.com/nestybox/sysbox)** — a container runtime that enables secure Docker-in-Docker without `--privileged`. Required on Linux for production-grade isolation. See the [Linux quickstart](quickstart-linux.md) for installation instructions.
  - **Supported platforms:** Ubuntu (18, 20, 22, 24), Debian (10, 11). Other distributions are also supported but require building sysbox from source. See the [sysbox distribution compatibility matrix](https://github.com/nestybox/sysbox/blob/master/docs/distro-compat.md) for the full list.
  - **Requirements:** amd64 or arm64 architecture, kernel > 5.19, Docker must be installed first.
