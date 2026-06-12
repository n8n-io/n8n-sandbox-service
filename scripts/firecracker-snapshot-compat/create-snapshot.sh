#!/usr/bin/env bash
# Creates a Firecracker snapshot with an optional CPU template variant.
set -euo pipefail

VARIANT="${1:-none}"
OUT_DIR="${2:-/tmp/fc-compat-snapshots/${VARIANT}}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"

case "$VARIANT" in
none)
	unset CPU_TEMPLATE CPU_CONFIG_FILE
	;;
T2 | T2S | T2CL | C3)
	export CPU_TEMPLATE="$VARIANT"
	unset CPU_CONFIG_FILE
	;;
helper-custom | helper-intel-only)
	unset CPU_TEMPLATE
	CPU_CONFIG_FILE="$(bash "${SCRIPT_DIR}/lib/resolve-helper-config.sh" "$VARIANT")"
	export CPU_CONFIG_FILE
	;;
no-xcrs)
	unset CPU_TEMPLATE
	export CPU_CONFIG_FILE="/tmp/fc-compat-cpu-configs/no-xcrs.json"
	mkdir -p "$(dirname "$CPU_CONFIG_FILE")"
	cat >"$CPU_CONFIG_FILE" <<'EOF'
{
  "kvm_capabilities": ["!56"]
}
EOF
	;;
*)
	echo "unknown variant: $VARIANT" >&2
	exit 1
	;;
esac

mkdir -p "$OUT_DIR"
sudo env \
	MEM_MIB="${MEM_MIB:-512}" \
	VCPUS="${VCPUS:-1}" \
	CPU_TEMPLATE="${CPU_TEMPLATE:-}" \
	CPU_CONFIG_FILE="${CPU_CONFIG_FILE:-}" \
	bash "${PROJECT_ROOT}/e2e/infra/scripts/create-golden-snapshot.sh" \
	--kernel /srv/firecracker/template/vmlinux \
	--ext4 /srv/firecracker/template/rootfs.ext4 \
	--daemon-bin "${PROJECT_ROOT}/bin/sandbox-daemon" \
	--out "$OUT_DIR"
sudo chown -R "$(id -un):$(id -gn)" "$OUT_DIR"

echo "$OUT_DIR"
