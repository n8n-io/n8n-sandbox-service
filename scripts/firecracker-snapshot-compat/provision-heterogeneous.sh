#!/usr/bin/env bash
# Provisions three Intel-only cohorts for CPU heterogeneity via SKU/region mix:
#   1. germanywestcentral, Standard_D4s_v3  (Intel Xeon, 3rd-gen D-series)
#   2. westeurope,         Standard_D4s_v4  (Intel Xeon, 4th-gen D-series)
#   3. germanywestcentral, Standard_D4s_v5  (Intel Xeon, 5th-gen D-series)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

STUDY_ID="${COMPAT_STUDY_ID:-fc-snap-hetero-${GITHUB_RUN_ID:-$(date +%s)}}"
INSTANCE_COUNT="${COMPAT_INSTANCE_COUNT:-3}"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
STATE_DIR="${OUT_DIR}/infra/state"
DEPLOYMENTS_FILE="${OUT_DIR}/deployments.json"
PROVISION="${ROOT}/scripts/firecracker-snapshot-compat/infra/provision-vmss.sh"

mkdir -p "$STATE_DIR" "${OUT_DIR}/fingerprints"

declare -a COHORT_SPECS=(
	"gwc-d4sv3:germanywestcentral:Standard_D4s_v3"
	"weu-d4sv4:westeurope:Standard_D4s_v4"
	"gwc-d4sv5:germanywestcentral:Standard_D4s_v5"
)

COHORT_INSTANCE_FILES=()
for spec in "${COHORT_SPECS[@]}"; do
	IFS=':' read -r cohort_id location vm_size <<<"$spec"
	deployment_name="${STUDY_ID}-${cohort_id}"
	echo "==> Provisioning cohort ${cohort_id} (${location}, ${vm_size})"

	COMPAT_DEPLOYMENT_NAME="$deployment_name" \
		COMPAT_LOCATION="$location" \
		COMPAT_VM_SIZE="$vm_size" \
		COMPAT_INSTANCE_COUNT="$INSTANCE_COUNT" \
		COMPAT_COHORT_ID="$cohort_id" \
		COMPAT_TF_STATE="${STATE_DIR}/${cohort_id}.tfstate" \
		COMPAT_FINGERPRINT_DIR="${OUT_DIR}/fingerprints/${cohort_id}" \
		COMPAT_INSTANCES_JSON="${OUT_DIR}/instances-${cohort_id}.json" \
		bash "$PROVISION"

	COHORT_INSTANCE_FILES+=("${OUT_DIR}/instances-${cohort_id}.json")
done

node "${ROOT}/scripts/firecracker-snapshot-compat/lib/merge-cohorts.js" \
	"$OUT_DIR" "$STUDY_ID" "$DEPLOYMENTS_FILE" \
	"${COHORT_INSTANCE_FILES[@]}"

bash "${ROOT}/scripts/firecracker-snapshot-compat/analyze-fingerprints.sh"
bash "${ROOT}/scripts/firecracker-snapshot-compat/sync-cpu-configs.sh" "${OUT_DIR}/instances.json"

echo "==> Heterogeneous study ${STUDY_ID} ready."
echo "    instances: ${OUT_DIR}/instances.json"
echo "    deployments: ${DEPLOYMENTS_FILE}"
