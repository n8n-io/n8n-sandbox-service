# Prerequisites

## Required

- **Docker** >= 24.x — the sandbox service runs as Docker containers and runners manage Docker-in-Docker sandbox lifecycles.

## Linux only

- **[sysbox-runc](https://github.com/nestybox/sysbox)** — a container runtime that enables secure Docker-in-Docker without `--privileged`. Required on Linux for production-grade isolation. See the [Linux quickstart](quickstart-linux.md) for installation instructions.
