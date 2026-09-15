# n8n Sandbox Service

Isolated, on-demand execution environments behind a REST API. Each sandbox is a container (Docker/Sysbox runner) or a Firecracker microVM, with a small HTTP daemon inside that runs commands and handles files. Runners register with a central API, which places sandboxes and proxies requests to them.

## Get started

| I want to… | Read |
| --- | --- |
| Run it on a Linux host with Sysbox | [docs/quickstart-linux.md](docs/quickstart-linux.md) |
| Run the Firecracker runner on a KVM host | [docs/quickstart-firecracker-linux.md](docs/quickstart-firecracker-linux.md) |
| Develop on macOS | [docs/quickstart-macos.md](docs/quickstart-macos.md) |
| Deploy to Kubernetes | [docs/quickstart-k8s.md](docs/quickstart-k8s.md) |
| Call the API | [docs/API.md](docs/API.md) · [TypeScript SDK](sdk/README.md) |
| Build and test | [docs/development.md](docs/development.md) |

The full index, including architecture, security model, configuration, operations and release process, is in [docs/README.md](docs/README.md).

## License

Source under the [Sustainable Use License](LICENSE.md). Files and directories with `.ee` in the name are under the [n8n Enterprise License](LICENSE_EE.md).
