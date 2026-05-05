#!/bin/sh
# Smoke test against the local API after `make up`: create a sandbox, stream exec
# output as pretty JSON, then delete the sandbox.
#
# Environment (optional):
#   SANDBOX_API_BASE  — API URL (default: http://localhost:8080)
#   SANDBOX_API_KEY   — X-Api-Key value (default: test, matching compose.yaml)
set -eu

BASE="${SANDBOX_API_BASE:-http://localhost:8080}"
KEY="${SANDBOX_API_KEY:-test}"

if ! command -v curl >/dev/null 2>&1; then
	echo "curl is required" >&2
	exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
	echo "jq is required" >&2
	exit 1
fi

echo "==> POST ${BASE}/sandboxes"
create_json="$(curl -fsS -X POST "${BASE}/sandboxes" \
	-H "X-Api-Key: ${KEY}" \
	-H "Content-Type: application/json" \
	-d '{}')"
echo "${create_json}" | jq .

sid="$(echo "${create_json}" | jq -r .id)"
if [ -z "${sid}" ] || [ "${sid}" = "null" ]; then
	echo "could not read sandbox id from create response" >&2
	exit 1
fi

echo "==> POST ${BASE}/sandboxes/${sid}/exec"
curl -fsSN -X POST "${BASE}/sandboxes/${sid}/exec" \
	-H "X-Api-Key: ${KEY}" \
	-H "Content-Type: application/json" \
	-d '{"command":"echo hello world","timeout_ms":60000}' |
	while IFS= read -r line || [ -n "${line}" ]; do
		[ -z "${line}" ] && continue
		printf '%s\n' "${line}" | jq .
	done

echo "==> DELETE ${BASE}/sandboxes/${sid}"
http_code="$(curl -fsS -o /dev/null -w "%{http_code}" -X DELETE "${BASE}/sandboxes/${sid}" -H "X-Api-Key: ${KEY}")"
jq -n --arg code "${http_code}" '{http_status: $code}'
