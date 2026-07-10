# Quickstart: Firecracker runner (Linux)

Run the API from Docker Hub and a Firecracker runner on a Linux host with KVM.
One runner process per machine; scale by adding hosts.

Full tarball contract: [BUNDLE.md](../BUNDLE.md).

## Requirements

- Ubuntu 22.04 or 24.04, amd64, `/dev/kvm`, root for the runner
- Outbound HTTPS (GitHub Releases, Docker Hub, Firecracker CI on S3)
- Go 1.25+ only if building the runner from source instead of pulling the Docker image

## 1. Golden-build bundle

Pick a [service release](https://github.com/n8n-io/n8n-sandbox-service/releases) version
(e.g. `1.1.0`). Download its tarball and build the host-local snapshot:

```bash
VERSION=1.1.0
curl -fsSL -o firecracker-golden-build.tar.gz \
  "https://github.com/n8n-io/n8n-sandbox-service/releases/download/service/v${VERSION}/firecracker-golden-build-${VERSION}.tar.gz"
tar xzf firecracker-golden-build.tar.gz
cd firecracker-golden-build

sudo ./scripts/install-runner-host.sh --download-ci-assets

source /srv/firecracker/ci-assets/manifest.env
sudo env \
  FIRECRACKER_CI_VMLINUX="$FIRECRACKER_CI_VMLINUX" \
  FIRECRACKER_CI_ROOTFS_SQUASHFS="$FIRECRACKER_CI_ROOTFS_SQUASHFS" \
  TEMPLATE_DIR=/srv/firecracker/template \
  ./scripts/build-rootfs-template.sh

sudo ./scripts/create-golden-snapshot.sh \
  --kernel /srv/firecracker/template/vmlinux \
  --ext4 /srv/firecracker/template/rootfs.ext4 \
  --daemon-bin ./bin/sandbox-daemon \
  --out /srv/firecracker/snapshots

sudo ln -sf snapshot_mem /srv/firecracker/snapshots/mem
sudo ln -sf snapshot_state /srv/firecracker/snapshots/state

export GIT_SHA="$(jq -r .git_sha MANIFEST.json)"
```

Rebuild the snapshot on each physical runner host (Firecracker snapshots are not portable).

## 2. Runner

### Option A: Docker Hub image (recommended)

```bash
docker pull "n8nio/n8n-sandbox-service-runner-firecracker:${VERSION}"
```

The image is linux/amd64 only and ships `sandbox-runner` plus Firecracker/jailer
binaries. Run on the host with `/dev/kvm`, snapshot paths mounted, and the same env
vars as section 4 (use the image entrypoint instead of `/usr/local/bin/sandbox-runner`).

### Option B: Build from source

Build at the same commit as the bundle:

```bash
git clone https://github.com/n8n-io/n8n-sandbox-service.git
cd n8n-sandbox-service
git checkout "$GIT_SHA"
make runner-firecracker
sudo install -m 0755 bin/runner-firecracker /usr/local/bin/sandbox-runner
```

## 3. API + mTLS (same host)

From the repo checkout (`n8n-sandbox-service/`):

```bash
export VERSION API_KEY=test SANDBOX_API_RUNNER_REGISTRATION_TOKEN=dev-reg-token
export SANDBOX_API_RUNNER_API_KEY=runner-test API_TLS_DNS=sandbox-api-local

curl -fsSL -o bootstrap-mtls.sh \
  https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/main/scripts/bootstrap-mtls.sh
chmod +x bootstrap-mtls.sh
./bootstrap-mtls.sh --out-dir .tls --api-san "$API_TLS_DNS" --control-sans localhost --force

docker run -d --name sandbox-api --network host \
  -v "$(pwd)/.tls/api:/tls:ro" \
  -e SANDBOX_API_LISTEN_ADDR=127.0.0.1:8080 \
  -e SANDBOX_API_GRPC_LISTEN_ADDR=127.0.0.1:9090 \
  -e SANDBOX_API_KEYS="$API_KEY" \
  -e SANDBOX_API_RUNNER_REGISTRATION_TOKEN="$SANDBOX_API_RUNNER_REGISTRATION_TOKEN" \
  -e SANDBOX_API_RUNNER_API_KEY="$SANDBOX_API_RUNNER_API_KEY" \
  -e SANDBOX_API_GRPC_TLS_CERT_FILE=/tls/grpc-server.crt \
  -e SANDBOX_API_GRPC_TLS_KEY_FILE=/tls/grpc-server.key \
  -e SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/tls/ca.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/tls/ca.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/tls/control-grpc-api-client.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/tls/control-grpc-api-client.key \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME=localhost \
  "n8nio/n8n-sandbox-service-api:${VERSION}"
```

## 4. Start the runner

```bash
TLS_DIR="$(pwd)/.tls"   # directory that contains api/ and runner/

sudo env \
  SANDBOX_RUNNER_BACKEND=firecracker \
  SANDBOX_RUNNER_ID="$(hostname)" \
  SANDBOX_RUNNER_LISTEN_ADDR=127.0.0.1:8081 \
  SANDBOX_RUNNER_HTTP_BASE_URL=http://127.0.0.1:8081 \
  SANDBOX_RUNNER_DATA_DIR=/var/sandboxes \
  SANDBOX_RUNNER_CAPACITY_TOTAL=10 \
  SANDBOX_RUNNER_API_KEYS="$SANDBOX_API_RUNNER_API_KEY" \
  SANDBOX_RUNNER_API_GRPC_ADDR=127.0.0.1:9090 \
  SANDBOX_RUNNER_REGISTRATION_TOKEN="$SANDBOX_API_RUNNER_REGISTRATION_TOKEN" \
  SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE="$TLS_DIR/runner/ca.crt" \
  SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE="$TLS_DIR/runner/grpc-client.crt" \
  SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE="$TLS_DIR/runner/grpc-client.key" \
  SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME="$API_TLS_DNS" \
  SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=127.0.0.1:9091 \
  SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=127.0.0.1:9091 \
  SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE="$TLS_DIR/runner/control-grpc-server.crt" \
  SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE="$TLS_DIR/runner/control-grpc-server.key" \
  SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE="$TLS_DIR/runner/ca.crt" \
  /usr/local/bin/sandbox-runner
```

Defaults for Firecracker paths (`/opt/firecracker/bin`, `/srv/firecracker/...`) match
`install-runner-host.sh`. Override via [configuration.md](configuration.md).

## 5. Verify

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8081/readyz

export SANDBOX_API_BASE=http://127.0.0.1:8080 SANDBOX_API_KEY="$API_KEY"
curl -fsS -H "X-Api-Key: $SANDBOX_API_KEY" -X POST "$SANDBOX_API_BASE/sandboxes" -d '{}'
```

Or run the full smoke script (from a repo checkout):

```bash
SANDBOX_API_BASE=http://127.0.0.1:8080 SANDBOX_API_KEY=test ./scripts/smoke-sandbox.sh
```

## Next steps

- [Configuration](configuration.md) — all runner and API env vars
- [REST API](API.md) — sandbox exec, files, lifecycle
- [Firecracker runtime notes](../internal/runner/runtime/firecracker.ee/README.md) — networking and limits
