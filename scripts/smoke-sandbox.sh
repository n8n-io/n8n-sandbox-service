#!/bin/sh
# Smoke test against a deployed or local sandbox API.
#
# Exercises core paths aligned with e2e (no idle TTL or other special config):
# create → exec → resolv.conf → DNS → HTTPS → file write/read → delete.
#
# Environment:
#   SANDBOX_API_BASE       — API base URL (required unless set in env file)
#   SANDBOX_API_KEY        — X-Api-Key (optional if kubectl secret vars are set)
#   SMOKE_ENV_FILE         — Env file to source (default: none)
#   SMOKE_DEV_ENV_FILE     — Alias for SMOKE_ENV_FILE (backward compatible)
#   SMOKE_ENV              — Load scripts/smoke-sandbox.<env>.env (e.g. dev, stage, prod)
#   KUBE_NAMESPACE         — Kubernetes namespace for API key secret
#   KUBE_AUTH_SECRET       — Secret name
#   KUBE_AUTH_SECRET_KEY   — Secret data key (comma-separated keys; first is used)
#   SMOKE_VERBOSE          — Set to 1 to print full exec NDJSON streams
#   SMOKE_EXEC_TIMEOUT_MS  — Exec timeout in ms (default: 60000)
#   SMOKE_EXTENDED         — Set to 1 to create a second sandbox (snapshot restore path)
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

if [ -n "${SMOKE_ENV:-}" ]; then
	PRESET_FILE="${SCRIPT_DIR}/smoke-sandbox.${SMOKE_ENV}.env"
	if [ -f "${PRESET_FILE}" ]; then
		SMOKE_ENV_FILE="${PRESET_FILE}"
	fi
fi

ENV_FILE="${SMOKE_ENV_FILE:-${SMOKE_DEV_ENV_FILE:-}}"
if [ -n "${ENV_FILE}" ] && [ -f "${ENV_FILE}" ]; then
	set -a
	# shellcheck disable=SC1090
	. "${ENV_FILE}"
	set +a
fi

BASE="${SANDBOX_API_BASE:-}"
KEY="${SANDBOX_API_KEY:-}"
EXEC_TIMEOUT_MS="${SMOKE_EXEC_TIMEOUT_MS:-60000}"
SMOKE_FILE_PATH="/tmp/n8n-sandbox-smoke.txt"
SMOKE_FILE_CONTENT="smoke-$(date -u +%Y%m%dT%H%M%SZ)"

if [ -z "${BASE}" ]; then
	echo "SANDBOX_API_BASE is required (set it directly or via SMOKE_ENV_FILE / SMOKE_ENV)" >&2
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

CURL_COMMON="-fsS"
case "${BASE}" in
https://*)
	CURL_COMMON="${CURL_COMMON} --tlsv1.2 --http1.1"
	;;
esac

if [ -z "${KEY}" ]; then
	if ! command -v kubectl >/dev/null 2>&1; then
		echo "SANDBOX_API_KEY or kubectl is required" >&2
		exit 1
	fi
	NAMESPACE="${KUBE_NAMESPACE:?KUBE_NAMESPACE is required when SANDBOX_API_KEY is not set}"
	AUTH_SECRET="${KUBE_AUTH_SECRET:?KUBE_AUTH_SECRET is required when SANDBOX_API_KEY is not set}"
	AUTH_SECRET_KEY="${KUBE_AUTH_SECRET_KEY:?KUBE_AUTH_SECRET_KEY is required when SANDBOX_API_KEY is not set}"
	KEY="$(kubectl -n "${NAMESPACE}" get secret "${AUTH_SECRET}" -o "jsonpath={.data.${AUTH_SECRET_KEY}}" | base64 -d | cut -d, -f1)"
	if [ -z "${KEY}" ]; then
		echo "could not read API key from secret/${AUTH_SECRET} key ${AUTH_SECRET_KEY}" >&2
		exit 1
	fi
fi

sid=""
sid2=""
tmp_files=""

cleanup() {
	for cleanup_sid in ${sid} ${sid2}; do
		[ -n "${cleanup_sid}" ] || continue
		# shellcheck disable=SC2086
		curl ${CURL_COMMON} -o /dev/null -X DELETE "${BASE}/sandboxes/${cleanup_sid}" -H "X-Api-Key: ${KEY}" >/dev/null 2>&1 || true
	done
	if [ -n "${tmp_files}" ]; then
		# shellcheck disable=SC2086
		rm -f ${tmp_files}
	fi
}
trap cleanup EXIT HUP INT TERM

mktemp_track() {
	local f
	f="$(mktemp)"
	tmp_files="${tmp_files} ${f}"
	printf '%s' "${f}"
}

step() {
	printf '==> %s\n' "$*"
}

fail() {
	printf 'smoke failed: %s\n' "$*" >&2
	exit 1
}

api_curl() {
	# shellcheck disable=SC2086
	curl ${CURL_COMMON} "$@" -H "X-Api-Key: ${KEY}"
}

api_json() {
	# shellcheck disable=SC2086
	curl ${CURL_COMMON} "$@" -H "X-Api-Key: ${KEY}" -H "Content-Type: application/json"
}

exec_exit_code() {
	local out="$1"
	jq -se 'map(select(.type=="exit")) | last | .exit_code // -1' "${out}"
}

exec_stdout() {
	local out="$1"
	jq -se 'map(select(.type=="stdout")) | map(.data) | join("")' "${out}"
}

trim_output() {
	printf '%s' "$1" | tr -d '\r\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//'
}

print_exec_verbose() {
	local out="$1"
	if [ "${SMOKE_VERBOSE:-0}" != "1" ]; then
		return 0
	fi
	while IFS= read -r line || [ -n "${line}" ]; do
		[ -z "${line}" ] && continue
		printf '%s\n' "${line}" | jq .
	done <"${out}"
}

assert_exec() {
	local label="$1"
	local command="$2"
	local want_exit="${3:-0}"
	local target_sid="${4:-$sid}"
	local out
	local got_exit got_stdout
	local attempt=0

	out="$(mktemp_track)"
	got_exit=""
	while [ "${attempt}" -lt 5 ]; do
		attempt=$((attempt + 1))
		if api_json -N -X POST "${BASE}/sandboxes/${target_sid}/executions" \
			-d "{\"command\":$(jq -n --arg c "${command}" '$c'),\"timeout_ms\":${EXEC_TIMEOUT_MS}}" >"${out}" \
			&& jq -se 'map(select(.type=="exit")) | length > 0' "${out}" >/dev/null 2>&1; then
			got_exit="$(trim_output "$(exec_exit_code "${out}")")"
			if [ "${got_exit}" = "${want_exit}" ]; then
				break
			fi
		fi
		sleep 2
	done

	print_exec_verbose "${out}"

	if [ "${got_exit:-}" != "${want_exit}" ]; then
		fail "${label}: exit ${got_exit:-<missing>}, want ${want_exit}"
	fi

	got_stdout="$(exec_stdout "${out}")"
	printf '    %s: ok' "${label}"
	if [ -n "${got_stdout}" ]; then
		printf ' (%s)' "$(printf '%s' "${got_stdout}" | tr '\n' ' ' | sed 's/  */ /g' | sed 's/ $//')"
	fi
	printf '\n'
}

create_sandbox() {
	local label="$1"
	local create_json="" attempt=0 new_sid=""

	printf '==> %s\n' "${label}" >&2
	while [ "${attempt}" -lt 5 ]; do
		attempt=$((attempt + 1))
		if create_json="$(api_json -X POST "${BASE}/sandboxes" -d '{}' 2>/dev/null)"; then
			break
		fi
		sleep 2
	done
	if [ -z "${create_json}" ]; then
		fail "${label}: request failed after retries"
	fi
	new_sid="$(printf '%s' "${create_json}" | jq -r .id)"
	if [ -z "${new_sid}" ] || [ "${new_sid}" = "null" ]; then
		fail "${label}: missing id"
	fi
	printf '    sandbox_id: %s\n' "${new_sid}" >&2
	printf '%s' "${new_sid}"
}

delete_sandbox() {
	local target_sid="$1"
	local label="$2"
	local http_code=""

	step "${label}"
	http_code="$(trim_output "$(api_curl -o /dev/null -w "%{http_code}" -X DELETE "${BASE}/sandboxes/${target_sid}")")"
	if [ "${http_code}" != "204" ]; then
		fail "${label}: HTTP ${http_code}, want 204"
	fi
	printf '    delete: %s\n' "${http_code}"
	if [ "${target_sid}" = "${sid}" ]; then
		sid=""
	fi
	if [ "${target_sid}" = "${sid2}" ]; then
		sid2=""
	fi
}

write_file() {
	local path="$1"
	local content="$2"
	local target_sid="${3:-$sid}"
	local http_code

	http_code="$(trim_output "$(
		printf '%s' "${content}" | api_curl -o /dev/null -w "%{http_code}" -X PUT \
			"${BASE}/sandboxes/${target_sid}/files?path=${path}" \
			-H "Content-Type: application/octet-stream" \
			--data-binary @-
	)")"
	if [ "${http_code}" != "200" ]; then
		fail "file write ${path}: HTTP ${http_code}, want 200"
	fi
}

read_file() {
	local path="$1"
	local target_sid="${2:-$sid}"
	api_curl "${BASE}/sandboxes/${target_sid}/files/content?path=${path}"
}

run_core_guest_checks() {
	local target_sid="$1"
	local prefix="${2:-}"

	assert_exec "${prefix}warm-up" "true" 0 "${target_sid}"
	assert_exec "${prefix}exec echo" "echo hello world" 0 "${target_sid}"
	assert_exec "${prefix}resolv.conf" "grep -E '^nameserver ' /etc/resolv.conf" 0 "${target_sid}"
	assert_exec "${prefix}dns resolve" "getent ahostsv4 example.com | head -1 | awk '{print \$1}'" 0 "${target_sid}"
	assert_exec "${prefix}https example.com" "curl -fsSL -o /dev/null --max-time 20 https://example.com/" 0 "${target_sid}"
}

step "healthz"
api_curl "${BASE}/healthz" >/dev/null

sid="$(create_sandbox "create sandbox")"
run_core_guest_checks "${sid}"

step "file write/read"
write_file "${SMOKE_FILE_PATH}" "${SMOKE_FILE_CONTENT}"
downloaded="$(read_file "${SMOKE_FILE_PATH}")"
if [ "${downloaded}" != "${SMOKE_FILE_CONTENT}" ]; then
	fail "file read mismatch (got ${downloaded:-<empty>})"
fi
printf '    file write/read: ok\n'

assert_exec "exec reads uploaded file" "cat ${SMOKE_FILE_PATH}"

delete_sandbox "${sid}" "delete sandbox"

if [ "${SMOKE_EXTENDED:-0}" = "1" ]; then
	sid2="$(create_sandbox "extended: create second sandbox")"
	run_core_guest_checks "${sid2}" "extended: "
	delete_sandbox "${sid2}" "extended: delete second sandbox"
fi

printf '==> smoke passed\n'
