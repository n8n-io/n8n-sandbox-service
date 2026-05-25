#!/bin/sh
# Smoke test against the public dev API: create a sandbox, stream exec output as
# pretty JSON, then delete it.
#
# Environment (optional):
#   SMOKE_DEV_ENV_FILE    — Env file to load (default: scripts/smoke-dev-sandbox.env)
#   KUBE_NAMESPACE        — Kubernetes namespace used to read the API key
#   KUBE_AUTH_SECRET      — Auth Secret name
#   KUBE_AUTH_SECRET_KEY  — Auth Secret key for API keys
#   SANDBOX_API_BASE      — API URL
#   SANDBOX_API_KEY       — X-Api-Key value (default: read from Kubernetes Secret)
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="${SMOKE_DEV_ENV_FILE:-${SCRIPT_DIR}/smoke-dev-sandbox.env}"

if [ -f "${ENV_FILE}" ]; then
	set -a
	. "${ENV_FILE}"
	set +a
fi

BASE="${SANDBOX_API_BASE:-}"

if [ -z "${BASE}" ]; then
	echo "SANDBOX_API_BASE is required; set it in ${ENV_FILE} or the environment" >&2
	exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
	echo "curl is required" >&2
	exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
	echo "jq is required" >&2
	exit 1
fi
if [ -z "${SANDBOX_API_KEY:-}" ] && ! command -v kubectl >/dev/null 2>&1; then
	echo "kubectl is required when SANDBOX_API_KEY is not set" >&2
	exit 1
fi

cleanup() {
	if [ -n "${sid:-}" ]; then
		curl -fsS --tlsv1.2 --http1.1 -o /dev/null -X DELETE "${BASE}/sandboxes/${sid}" -H "X-Api-Key: ${KEY:-}" >/dev/null 2>&1 || true
	fi
	if [ -n "${tmp_exec_out:-}" ]; then
		rm -f "${tmp_exec_out}"
	fi
}
trap cleanup EXIT HUP INT TERM

if [ -n "${SANDBOX_API_KEY:-}" ]; then
	KEY="${SANDBOX_API_KEY}"
else
	NAMESPACE="${KUBE_NAMESPACE:?KUBE_NAMESPACE is required when SANDBOX_API_KEY is not set}"
	AUTH_SECRET="${KUBE_AUTH_SECRET:?KUBE_AUTH_SECRET is required when SANDBOX_API_KEY is not set}"
	AUTH_SECRET_KEY="${KUBE_AUTH_SECRET_KEY:?KUBE_AUTH_SECRET_KEY is required when SANDBOX_API_KEY is not set}"
	KEY="$(kubectl -n "${NAMESPACE}" get secret "${AUTH_SECRET}" -o "jsonpath={.data.${AUTH_SECRET_KEY}}" | base64 -d | cut -d, -f1)"
	if [ -z "${KEY}" ]; then
		echo "could not read API key from secret/${AUTH_SECRET} key ${AUTH_SECRET_KEY}" >&2
		exit 1
	fi
fi

if ! curl -fsS --tlsv1.2 --http1.1 "${BASE}/healthz" >/dev/null 2>&1; then
	echo "API is not reachable at ${BASE}" >&2
	exit 1
fi

echo "==> POST ${BASE}/sandboxes"
create_json="$(curl -fsS --tlsv1.2 --http1.1 -X POST "${BASE}/sandboxes" \
	-H "X-Api-Key: ${KEY}" \
	-H "Content-Type: application/json" \
	-d '{}')"
echo "${create_json}" | jq .

sid="$(echo "${create_json}" | jq -r .id)"
if [ -z "${sid}" ] || [ "${sid}" = "null" ]; then
	echo "could not read sandbox id from create response" >&2
	exit 1
fi

echo "==> POST ${BASE}/sandboxes/${sid}/executions"
tmp_exec_out="$(mktemp)"
curl -fsSN --tlsv1.2 --http1.1 -X POST "${BASE}/sandboxes/${sid}/executions" \
	-H "X-Api-Key: ${KEY}" \
	-H "Content-Type: application/json" \
	-d '{"command":"echo hello world","timeout_ms":60000}' >"${tmp_exec_out}"

while IFS= read -r line || [ -n "${line}" ]; do
	[ -z "${line}" ] && continue
	printf '%s\n' "${line}" | jq .
done <"${tmp_exec_out}"

rm -f "${tmp_exec_out}"
tmp_exec_out=""

echo "==> DELETE ${BASE}/sandboxes/${sid}"
http_code="$(curl -fsS --tlsv1.2 --http1.1 -o /dev/null -w "%{http_code}" -X DELETE "${BASE}/sandboxes/${sid}" -H "X-Api-Key: ${KEY}")"
sid=""
jq -n --arg code "${http_code}" '{http_status: $code}'
