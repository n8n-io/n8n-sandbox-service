#!/usr/bin/env bash
# Destroys all cohort deployments from a heterogeneous study.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
DEPLOYMENTS_FILE="${OUT_DIR}/deployments.json"
DESTROY="${ROOT}/scripts/firecracker-snapshot-compat/infra/destroy-deployment.sh"

if [[ ! -f "$DEPLOYMENTS_FILE" ]]; then
	echo "ERROR: ${DEPLOYMENTS_FILE} not found" >&2
	exit 1
fi

COHORT_COUNT="$(node -e 'process.stdout.write(String(require(process.argv[1]).cohorts.length))' "$DEPLOYMENTS_FILE")"
RESOURCE_GROUP="$(node -e 'process.stdout.write(require(process.argv[1]).resource_group)' "$DEPLOYMENTS_FILE")"

for i in $(seq 0 $((COHORT_COUNT - 1))); do
	deployment_name="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.cohorts[Number(process.argv[2])].deployment_name)' "$DEPLOYMENTS_FILE" "$i")"
	cohort_id="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.cohorts[Number(process.argv[2])].cohort_id)' "$DEPLOYMENTS_FILE" "$i")"
	echo "==> Destroying cohort ${cohort_id} (${deployment_name})"
	RESOURCE_GROUP="$RESOURCE_GROUP" DEPLOYMENT_NAME="$deployment_name" \
		bash "$DESTROY" --keep-local-state "$@"
done

rm -f "$DEPLOYMENTS_FILE" "${OUT_DIR}"/instances-*.json
echo "==> Heterogeneous study destroyed"
