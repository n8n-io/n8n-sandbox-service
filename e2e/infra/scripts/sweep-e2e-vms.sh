#!/usr/bin/env bash
# Finds and deletes all Azure resources tagged with purpose=sandbox-service-e2e.
# Intended as a scheduled safety net to clean up leaked VMs from failed
# e2e workflow runs. Requires: RESOURCE_GROUP env var and authenticated az CLI.
set -euo pipefail

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TAG="purpose=sandbox-service-e2e"
CUTOFF_EPOCH=$(python3 - <<'PY'
import time

print(int(time.time()) - 3600)
PY
)

iso_to_epoch() {
	python3 - "$1" <<'PY'
import datetime
import sys

value = sys.argv[1]
if value.endswith("Z"):
    value = f"{value[:-1]}+00:00"
dt = datetime.datetime.fromisoformat(value)
if dt.tzinfo is None:
    dt = dt.replace(tzinfo=datetime.timezone.utc)
print(int(dt.timestamp()))
PY
}

echo "==> Finding VMs tagged ${TAG} in ${RESOURCE_GROUP}..."
VM_IDS=$(az vm list \
	--resource-group "$RESOURCE_GROUP" \
	--query "[?tags.purpose=='sandbox-service-e2e'].id" \
	-o tsv)

if [[ -z "$VM_IDS" ]]; then
	echo "No e2e VMs found. Nothing to clean up."
	exit 0
fi

VM_COUNT=$(echo "$VM_IDS" | wc -l | tr -d ' ')
echo "==> Found ${VM_COUNT} tagged VM(s); deleting only VMs older than 1 hour"

for VM_ID in $VM_IDS; do
	VM_NAME=$(az vm show --ids "$VM_ID" --query "name" -o tsv)
	VM_CREATED_AT=$(az vm show --ids "$VM_ID" --query "timeCreated" -o tsv 2>/dev/null) || true
	if [[ -z "$VM_CREATED_AT" || "$VM_CREATED_AT" == "null" ]]; then
		echo "==> Skipping ${VM_NAME}; VM creation time is unavailable"
		continue
	fi
	VM_CREATED_EPOCH=$(iso_to_epoch "$VM_CREATED_AT" 2>/dev/null) || {
		echo "==> Skipping ${VM_NAME}; could not parse creation time: ${VM_CREATED_AT}"
		continue
	}
	if [[ "$VM_CREATED_EPOCH" -gt "$CUTOFF_EPOCH" ]]; then
		echo "==> Skipping ${VM_NAME}; VM is less than 1 hour old"
		continue
	fi

	echo "==> Deleting VM ${VM_NAME} and associated resources..."

	NIC_ID=$(az vm show --ids "$VM_ID" --query 'networkProfile.networkInterfaces[0].id' -o tsv 2>/dev/null) || true
	OS_DISK_ID=$(az vm show --ids "$VM_ID" --query 'storageProfile.osDisk.managedDisk.id' -o tsv 2>/dev/null) || true

	PUBLIC_IP_ID=""
	NSG_ID=""
	SUBNET_ID=""
	VNET_ID=""
	if [[ -n "$NIC_ID" ]]; then
		PUBLIC_IP_ID=$(az network nic show --ids "$NIC_ID" --query 'ipConfigurations[0].publicIPAddress.id' -o tsv 2>/dev/null) || true
		NSG_ID=$(az network nic show --ids "$NIC_ID" --query 'networkSecurityGroup.id' -o tsv 2>/dev/null) || true
		SUBNET_ID=$(az network nic show --ids "$NIC_ID" --query 'ipConfigurations[0].subnet.id' -o tsv 2>/dev/null) || true
		if [[ -n "$SUBNET_ID" ]]; then
			VNET_ID="${SUBNET_ID%/subnets/*}"
		fi
	fi

	az vm delete --ids "$VM_ID" --yes --force-deletion none 2>/dev/null || true
	[[ -n "$NIC_ID" ]] && az network nic delete --ids "$NIC_ID" 2>/dev/null || true
	[[ -n "$PUBLIC_IP_ID" ]] && az network public-ip delete --ids "$PUBLIC_IP_ID" 2>/dev/null || true
	[[ -n "$OS_DISK_ID" ]] && az disk delete --ids "$OS_DISK_ID" --yes 2>/dev/null || true
	[[ -n "$NSG_ID" ]] && az network nsg delete --ids "$NSG_ID" 2>/dev/null || true
	[[ -n "$VNET_ID" ]] && az network vnet delete --ids "$VNET_ID" 2>/dev/null || true

	echo "==> Deleted ${VM_NAME}"
done

echo "==> Sweep complete"
