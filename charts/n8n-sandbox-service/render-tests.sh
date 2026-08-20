#!/usr/bin/env bash
# Render tests for the n8n-sandbox-service chart. CI runs this script.
# Run it locally from any directory:
#
#   charts/n8n-sandbox-service/render-tests.sh
#
# The tests only render templates with helm; they need no cluster.
set -euo pipefail

CHART_DIR=$(cd "$(dirname "$0")" && pwd)

render() {
	helm template n8n-sandbox-service "$CHART_DIR" \
		--set auth.existingSecret=n8n-sandbox-auth "$@"
}

must_fail() {
	local message=$1
	shift
	local out
	if out=$(render "$@" 2>&1); then
		echo "expected the render to fail: $*" >&2
		exit 1
	fi
	if ! printf '%s' "$out" | grep -q "$message"; then
		echo "expected failure message [$message] for: $*" >&2
		printf '%s\n' "$out" >&2
		exit 1
	fi
}

privileged=(--set runner.isolation=privileged --set runner.acknowledgePrivileged=true)

echo "==> render variants"
render >/dev/null
render --set dataPlane.mode=external >/dev/null
render \
	--set tls.mode=certManager \
	--set tls.certManager.issuerRef.name=sandbox-ca \
	--set tls.certManager.issuerRef.kind=ClusterIssuer >/dev/null
render \
	--set api.ingress.enabled=true \
	--set 'api.ingress.hosts[0].host=sandbox.example.com' \
	--set 'api.ingress.tls[0].secretName=sandbox-api-public-tls' \
	--set 'api.ingress.tls[0].hosts[0]=sandbox.example.com' >/dev/null
render --set networkPolicy.enabled=true >/dev/null
render --set dataPlane.mode=external --set networkPolicy.enabled=true >/dev/null
render "${privileged[@]}" >/dev/null
render "${privileged[@]}" --set networkPolicy.enabled=true >/dev/null

echo "==> quota pool image lands on the bounded volume"
# Privileged isolation: the pool lands on the bounded emptyDir.
manifests=$(render "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60)
echo "$manifests" | grep -q 'mountPath: "/var/lib/docker-pool"'
echo "$manifests" | grep -q 'SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH: "/var/lib/docker-pool/docker.img"'

# Sysbox isolation: the README example with persistence works the same.
manifests=$(render \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60 \
	--set runner.dockerDataRoot.persistence.enabled=true \
	--set runner.dockerDataRoot.persistence.size=64Gi)
echo "$manifests" | grep -q 'mountPath: "/var/lib/docker-pool"'
echo "$manifests" | grep -q 'SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH: "/var/lib/docker-pool/docker.img"'

# A pool larger than the volume must fail the render.
must_fail "must not exceed the docker data root size" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=100

# The pool size must be a positive integer.
must_fail "positive whole number" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=0
must_fail "positive whole number" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=-5
must_fail "positive whole number" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048

# A missing or unsupported volume size must fail, not skip the guard.
must_fail "whole number of G or Gi" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60 \
	--set runner.dockerDataRoot.emptyDir.sizeLimit=null
must_fail "whole number of G or Gi" "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60 \
	--set runner.dockerDataRoot.emptyDir.sizeLimit=500Mi

# An explicit pool path owns its own volume; skip the size comparison.
manifests=$(render "${privileged[@]}" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=100 \
	--set runner.config.diskQuotaPoolPath=/mnt/pool/docker.img)
echo "$manifests" | grep -q 'SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH: "/mnt/pool/docker.img"'

echo "==> long runner names stay isolation-specific and unique"
statefulset_name() {
	local release_name=$1
	shift
	helm template "$release_name" "$CHART_DIR" \
		--set auth.existingSecret=n8n-sandbox-auth "$@" \
		--show-only templates/runner-statefulset.yaml |
		awk '/^  name:/{print $2; exit}'
}
release_name=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
other_release_name=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab
sysbox_name=$(statefulset_name "$release_name")
privileged_name=$(statefulset_name "$release_name" "${privileged[@]}")
other_privileged_name=$(statefulset_name "$other_release_name" "${privileged[@]}")
test "$sysbox_name" != "$privileged_name"
test "$privileged_name" != "$other_privileged_name"

echo "render tests passed"
