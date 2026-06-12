#!/usr/bin/env bash
# Resolves or builds a helper CPU config file for snapshot create/restore variants.
set -euo pipefail

VARIANT="${1:?variant required}"
_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "$_LIB_DIR/../../.." && pwd)}"
COMPAT_DIR="${COMPAT_OUT_DIR:-$PROJECT_ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${COMPAT_INSTANCES_JSON:-$COMPAT_DIR/instances.json}"

case "$VARIANT" in
helper-custom)
	CONFIG_FILE="/tmp/fc-compat-cpu-configs/helper-custom.json"
	BUILD_ARGS=(--compat-dir "$COMPAT_DIR")
	;;
helper-intel-only)
	CONFIG_FILE="/tmp/fc-compat-cpu-configs/helper-intel-only.json"
	BUILD_ARGS=(--intel-only --representatives --compat-dir "$COMPAT_DIR" --instances "$INSTANCES_JSON")
	;;
*)
	echo "unknown helper variant: $VARIANT" >&2
	exit 1
	;;
esac

if [[ -f "$CONFIG_FILE" ]]; then
	printf '%s' "$CONFIG_FILE"
	exit 0
fi

mkdir -p "$(dirname "$CONFIG_FILE")"
if [[ ! -f "$CONFIG_FILE" ]]; then
	bash "${PROJECT_ROOT}/scripts/firecracker-snapshot-compat/build-helper-custom-config.sh" \
		--out "$CONFIG_FILE" \
		"${BUILD_ARGS[@]}"
fi

printf '%s' "$CONFIG_FILE"
