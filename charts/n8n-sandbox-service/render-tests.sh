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

# Default sysbox isolation has no volume for the pool image. Quotas must not
# render, because the runner would put a pool sized from capacityTotal on the
# container filesystem.
must_fail "needs a bounded volume" \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60
must_fail "needs a bounded volume" \
	--set runner.config.defaultDiskQuotaMb=2048

# An explicit pool path covers that case: the operator owns the volume.
manifests=$(render \
	--set runner.config.defaultDiskQuotaMb=2048 \
	--set runner.config.diskQuotaPoolSizeGb=60 \
	--set runner.config.diskQuotaPoolPath=/mnt/pool/docker.img)
echo "$manifests" | grep -q 'SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH: "/mnt/pool/docker.img"'
echo "$manifests" | grep -q 'SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB: "60"'

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

echo "==> runner names leave room for the StatefulSet ordinal"
# The StatefulSet controller copies the pod name "<runner>-<ordinal>" into the
# pod hostname, and Kubernetes limits a hostname to 63 characters. A runner
# name above 61 characters therefore renders a StatefulSet whose pods the API
# server always rejects. Helm limits a release name to 53 characters.
for length in 1 20 40 41 42 43 44 45 46 47 48 49 50 53; do
	long_release=$(printf 'a%.0s' $(seq "$length"))
	for isolation in sysbox privileged; do
		name=$(statefulset_name "$long_release" \
			--set runner.isolation="$isolation" \
			--set runner.acknowledgePrivileged=true)
		if test "${#name}" -gt 61; then
			echo "runner name leaves no room for the ordinal:" >&2
			echo "  isolation=$isolation release length=$length name=$name (${#name})" >&2
			exit 1
		fi
	done
done

echo "==> the runner scrape verifies TLS by default"
# The default must pin serverName to a name the runner certificate actually
# carries, or the scrape fails to verify. Compare against the issued SANs
# rather than a literal, so renaming the runner cannot drift the two apart.
cert_manager=(
	--set tls.mode=certManager
	--set tls.certManager.issuerRef.name=sandbox-ca
	--set tls.certManager.issuerRef.kind=ClusterIssuer
)
monitor=$(render --set monitoring.serviceMonitor.enabled=true "${cert_manager[@]}" \
	--show-only templates/servicemonitor.yaml)
echo "$monitor" | grep -q 'key: "ca.crt"'
server_name=$(echo "$monitor" | sed -n 's/.*serverName: "\(.*\)"/\1/p')
test -n "$server_name"
render "${cert_manager[@]}" --show-only templates/cert-manager.yaml |
	grep -q -- "- $server_name\$"

echo "==> the runner scrape TLS config stays overridable"
render --set monitoring.serviceMonitor.enabled=true \
	--set monitoring.serviceMonitor.runner.tlsConfig.insecureSkipVerify=true \
	--show-only templates/servicemonitor.yaml | grep -q 'insecureSkipVerify: true'
# A plaintext scrape must not carry a tlsConfig.
if render --set monitoring.serviceMonitor.enabled=true \
	--set monitoring.serviceMonitor.runner.scheme=http \
	--show-only templates/servicemonitor.yaml | grep -q 'tlsConfig'; then
	echo "a plaintext runner scrape must not render a tlsConfig" >&2
	exit 1
fi

echo "render tests passed"
