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

Build all four service binaries (compiled for Linux):

```bash
make all
```

Or build individually:

```bash
make api                 # Public API gateway
make runner-docker       # Docker/sysbox runner
make runner-firecracker  # Firecracker runner (make runner builds both)
make daemon              # In-container HTTP daemon (static, CGO_ENABLED=0)
```

Binaries are written to `bin/`.

The Go version is the `go` line in `go.mod`. GitHub workflows and the e2e VM setup scripts install exactly that version, so it also builds the golden-build `sandbox-daemon`; an older local Go fetches it automatically. The Dockerfiles use the same minor (`golang:1.27-bookworm`), so a minor bump changes them too.

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

Quick smoke test (create, exec, resolv.conf, DNS, HTTPS, file write/read, delete, then a sandbox with `egress: public` and one with `egress: none` whose outbound traffic must be blocked):

```bash
make smoke
```

Against a deployed environment, copy `scripts/smoke-sandbox.env.example` to
`scripts/smoke-sandbox.<env>.env` (gitignored) and run:

```bash
SMOKE_ENV=<env> sh scripts/smoke-sandbox.sh
SMOKE_ENV=<env> SMOKE_EXTENDED=1 sh scripts/smoke-sandbox.sh   # Firecracker: also a second create (snapshot restore path)
```

## Tests

Run Go unit tests:

```bash
make test
```

The Postgres store and registry tests are behind the `integration` build tag and need a database. `make test-integration` runs them and fails when `SANDBOX_TEST_POSTGRES_HOST` is unset (`SANDBOX_TEST_POSTGRES_{PORT,USER,PASSWORD,DB,SSLMODE}` are optional). Use a throwaway database: the tests run the migrations and write runners and sandboxes that a running API would act on. CI runs them against `postgres:16-alpine`:

```bash
docker run -d --rm --name sandbox-test-postgres -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:5432:5432 postgres:16-alpine
# -h: on first start the image runs a temporary server on the Unix socket only.
for i in $(seq 30); do docker exec sandbox-test-postgres pg_isready -h 127.0.0.1 -U postgres -q && break; sleep 1; done
SANDBOX_TEST_POSTGRES_HOST=127.0.0.1 SANDBOX_TEST_POSTGRES_PASSWORD=postgres make test-integration
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

A multi-hour execution on the factory pool's exec cap is opt-in: `E2E_LONG_EXEC_HOURS=5 E2E_RUNNER_MAX_EXEC_TIMEOUT=8h ./e2e/run.sh --grep "Long execution"`.

Opt-in settings for `e2e/run.sh`, none set by default:

- `E2E_KEEP_UP=1`: start the stack, print its API URL, admin key, container, network, registry and secrets directory, and wait for SIGINT or SIGTERM instead of running the tests; the signal tears it down.
- `E2E_USAGE_SINK_URL`: the runner's usage sink, so `model_hosts` creates are served. `E2E_API_MAX_IDLE_WINDOW` (e.g. `168h`): the API's idle window cap.
- `E2E_RUNNER_TEMPLATES` and `E2E_API_TEMPLATES`: the runner's `SANDBOX_RUNNER_DOCKER_TEMPLATES` JSON, with `{registry}` for the e2e registry, and its names for the API, replacing the `small` template. `E2E_PUSH_IMAGES` (space-separated local images) are pushed there first, as `{registry}/<last path segment>`.
- `E2E_EGRESS_PROXY_INTERNAL_HOSTS`: hosts the egress proxy may reach in private ranges, e.g. services on a network you `docker network connect` the runner container to.
- `E2E_RUNNER_DISK_QUOTA_MB` (default `50`) and `E2E_RUNNER_DISK_POOL_GB` (default `4`): per-sandbox disk and the runner's pool.

Ports and the registry are set with `PORT`, `RUNNER_CONTROL_HOST_PORT`, `REGISTRY_PORT` and `N8N_SANDBOX_REGISTRY_NAME`; containers and the network are named after the script's pid.

Idle-TTL, Postgres, multi-pod and Firecracker lanes: [e2e/README.md](../e2e/README.md).

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

After modifying shell scripts, always run:

```bash
make shell-fmt-check    # Check that all shell scripts are shfmt-formatted
make shell-lint         # Run shellcheck on all shell scripts
```

To auto-format:

```bash
make shell-fmt
```

These need `shfmt` and `shellcheck` (`brew install shfmt shellcheck`). CI pins
shfmt v3.13.1 and shellcheck v0.11.0; older shellcheck releases report findings
that v0.11.0 has since dropped, so match the pinned version locally.
