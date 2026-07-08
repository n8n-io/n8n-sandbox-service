#!/usr/bin/env bash
# Restarts the Firecracker runner started by e2e/run-firecracker.sh.
# Requires E2E_FIRECRACKER_RUNNER_ENV_FILE (written by the harness).
set -euo pipefail

if [[ -z "${E2E_FIRECRACKER_RUNNER_ENV_FILE:-}" || ! -f "$E2E_FIRECRACKER_RUNNER_ENV_FILE" ]]; then
	echo "E2E_FIRECRACKER_RUNNER_ENV_FILE must point to the runner env file from run-firecracker.sh" >&2
	exit 1
fi

# shellcheck disable=SC1090
source "$E2E_FIRECRACKER_RUNNER_ENV_FILE"

if [[ -n "${E2E_RUNNER_PID:-}" ]]; then
	sudo kill "$E2E_RUNNER_PID" >/dev/null 2>&1 || true
	wait "$E2E_RUNNER_PID" >/dev/null 2>&1 || true
fi

sudo rm -rf /srv/jailer/firecracker/sandbox-* >/dev/null 2>&1 || true
for i in $(seq 0 63); do sudo ip link delete "fc-veth-${i}" >/dev/null 2>&1 || true; done
sudo ip netns list | awk '{print $1}' | grep '^fc-sb-' | xargs -r -n1 sudo ip netns delete || true

sudo env "${RUNNER_ENV[@]}" "${RUNNER_BIN}" >"$RUNNER_LOG" 2>&1 &
E2E_RUNNER_PID=$!
export E2E_RUNNER_PID

wait_for_http() {
	local name=$1 url=$2
	for _ in $(seq 1 60); do
		if curl -sf "$url" >/dev/null 2>&1; then
			echo "${name} is ready."
			return 0
		fi
		sleep 1
	done
	echo "${name} failed to become ready: ${url}" >&2
	return 1
}

wait_for_http "Firecracker runner" "http://${RUNNER_ADDR}/readyz"

# Persist the new pid for subsequent restarts in the same e2e session.
{
	echo "export E2E_RUNNER_PID=${E2E_RUNNER_PID}"
} >>"$E2E_FIRECRACKER_RUNNER_ENV_FILE"

echo "$E2E_RUNNER_PID"
