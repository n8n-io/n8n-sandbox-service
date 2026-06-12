#!/usr/bin/env bash
# Builds a custom Firecracker CPU config from Intel-only host fingerprints
# (one representative per CPU signature when instances.json is available).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${COMPAT_INSTANCES_JSON:-$OUT_DIR/instances.json}"

exec bash "$(dirname "$0")/build-helper-custom-config.sh" \
	--intel-only \
	--representatives \
	--compat-dir "$OUT_DIR" \
	--instances "$INSTANCES_JSON" \
	"$@"
