#!/usr/bin/env bash
# Builds a custom Firecracker CPU config from instance fingerprints.
# Uses cpu-template-helper when installed; otherwise emits a minimal fallback.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FINGERPRINT_SOURCES="${SCRIPT_DIR}/lib/fingerprint-sources.js"

OUT_FILE=""
COMPAT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON=""
INTEL_ONLY=0
REPRESENTATIVES=0
FINGERPRINTS=()

usage() {
	cat >&2 <<EOF
Usage: $0 --out PATH [FINGERPRINT.json ...]
       $0 --out PATH --compat-dir DIR [--intel-only] [--representatives] [--instances PATH]

When no fingerprint files are passed, discovers them under <compat-dir>/fingerprints/.
--intel-only excludes AMD hosts; --representatives keeps one file per Intel CPU signature.
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--out)
		OUT_FILE="$2"
		shift 2
		;;
	--compat-dir)
		COMPAT_DIR="$2"
		shift 2
		;;
	--instances)
		INSTANCES_JSON="$2"
		shift 2
		;;
	--intel-only)
		INTEL_ONLY=1
		shift
		;;
	--representatives)
		REPRESENTATIVES=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		usage
		echo "unknown argument: $1" >&2
		exit 1
		;;
	*)
		FINGERPRINTS+=("$1")
		shift
		;;
	esac
done

if [[ -z "$OUT_FILE" ]]; then
	usage
	exit 1
fi

if [[ ${#FINGERPRINTS[@]} -eq 0 ]]; then
	discover_args=(discover "$COMPAT_DIR")
	[[ "$INTEL_ONLY" == "1" ]] && discover_args+=(--intel-only)
	[[ "$REPRESENTATIVES" == "1" ]] && discover_args+=(--representatives)
	if [[ -n "$INSTANCES_JSON" ]]; then
		discover_args+=(--instances "$INSTANCES_JSON")
	fi
	while IFS= read -r fp; do
		[[ -n "$fp" ]] && FINGERPRINTS+=("$fp")
	done < <(node "$FINGERPRINT_SOURCES" "${discover_args[@]}")
fi

if [[ ${#FINGERPRINTS[@]} -eq 0 ]]; then
	echo "ERROR: no fingerprint files found for ${OUT_FILE}" >&2
	exit 1
fi

echo "==> Building CPU config from ${#FINGERPRINTS[@]} fingerprint(s): ${OUT_FILE}" >&2
printf '    %s\n' "${FINGERPRINTS[@]}" >&2

HELPER="${CPU_TEMPLATE_HELPER:-}"
if [[ -z "$HELPER" ]]; then
	for candidate in \
		/opt/firecracker/bin/cpu-template-helper \
		"${HOME}/.local/bin/cpu-template-helper" \
		/usr/local/bin/cpu-template-helper; do
		if [[ -x "$candidate" ]]; then
			HELPER="$candidate"
			break
		fi
	done
fi
HELPER="${HELPER:-/opt/firecracker/bin/cpu-template-helper}"
if [[ -x "$HELPER" ]]; then
	tmp_dir="$(mktemp -d)"
	trap 'rm -rf "$tmp_dir"' EXIT
	idx=0
	for fp in "${FINGERPRINTS[@]}"; do
		cp "$fp" "$tmp_dir/fingerprint-${idx}.json"
		idx=$((idx + 1))
	done
	"$HELPER" static --input-dir "$tmp_dir" --output "$OUT_FILE"
	echo "==> Wrote helper CPU config to ${OUT_FILE}"
	exit 0
fi

echo "WARN: cpu-template-helper not found at ${HELPER}; writing minimal fallback config" >&2
cat >"$OUT_FILE" <<'EOF'
{
  "kvm_capabilities": ["!56"]
}
EOF
echo "==> Wrote fallback CPU config to ${OUT_FILE}"
