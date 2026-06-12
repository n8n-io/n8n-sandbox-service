#!/usr/bin/env bash
# Destroys a Firecracker snapshot-compat deployment.
# Prefers Azure CLI cleanup so legacy VMSS stacks still tear down after Terraform changes.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

exec bash "${SCRIPT_DIR}/destroy-deployment.sh" "$@"
