# Development

## Contents

- [Building from source](#building-from-source)
- [Building Docker images](#building-docker-images)
- [Running locally](#running-locally)
- [Tests](#tests)
- [Playground](#playground)
- [SDK](#sdk)
- [Code formatting and linting](#code-formatting-and-linting)

## Building from source

Build all three service binaries (compiled for Linux):

```bash
make all
```

Or build individually:

```bash
make api       # Public API gateway
make runner    # Container lifecycle manager
make daemon    # In-container HTTP daemon (static, CGO_ENABLED=0)
```

Binaries are written to `bin/`.

## Building Docker images

Build all images for the current architecture:

```bash
make docker-local
```

Cross-compile for a specific architecture:

```bash
make docker-amd64    # API + runner + sandbox for linux/amd64
make docker-arm64    # API + runner + sandbox for linux/arm64
```

Individual image targets are also available (e.g. `make docker-api-amd64`, `make docker-runner-arm64`, `make docker-sandbox-amd64`). The Firecracker runner is amd64-only and can be built with `make docker-firecracker-runner-amd64`.

## Running locally

Start the full stack (API + two runners) with Docker Compose. This bootstraps local mTLS certificates, builds images, and starts services with the correct platform overlay:

```bash
make up
```

Stop all services:

```bash
make down
```

Quick smoke test (creates a sandbox, runs `echo hello world`, prints JSON):

```bash
make smoke
```

## Tests

Run Go unit tests:

```bash
make test
```

Run the full end-to-end suite (all topologies sequentially):

```bash
./e2e/run-all.sh
```

Run a single topology:

```bash
./e2e/run.sh              # Single runner + full Playwright suite
./e2e/run-no-runner.sh    # API only — expects POST /sandboxes to return 503
./e2e/run-two-runners.sh  # Two runners — placement routing per sandbox
```

Extra Playwright arguments can be appended (e.g. `./e2e/run-all.sh --grep pattern`).

`run-all.sh` runs `make docker-local` once. Per-topology scripts skip rebuilding when invoked from it (`E2E_SKIP_BUILD=1`). To rebuild before each phase, run topology scripts individually.

See [e2e/README.md](../e2e/README.md) for details on specs, workers, and how `resilience.spec.ts` uses the host Docker CLI.

## Playground

Start the browser-based testing UI at `http://localhost:5173`:

```bash
make playground
```

The playground lets you create sandboxes, execute commands, and browse files interactively. It requires the service to be running (`make up`).

## SDK

The TypeScript/JavaScript client library lives in `sdk/`. Run the full SDK pipeline:

```bash
make sdk    # install + build + typecheck + test
```

Individual targets:

```bash
make sdk-install      # pnpm install
make sdk-build        # pnpm build
make sdk-typecheck    # pnpm typecheck
make sdk-test         # pnpm test
make sdk-fmt          # Format with oxfmt
make sdk-fmt-check    # Check formatting
make sdk-lint         # Lint with oxlint
```

## Code formatting and linting

After modifying Go files, always run:

```bash
make fmt-check    # Check that all Go files are gofmt-formatted
make vet          # Run go vet on all packages
```

To auto-format:

```bash
make fmt
```
