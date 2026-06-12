#!/usr/bin/env bash
# Annotates instances.json and writes results/<study_id>/run-manifest.json from fingerprints.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"

node "${ROOT}/scripts/firecracker-snapshot-compat/lib/fingerprint-summary.js" analyze "$OUT_DIR"
