#!/usr/bin/env bash
# Deletes a Firecracker snapshot-compat deployment from Azure by name.
# Works without Terraform state (including legacy VMSS+LB stacks).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
TF_DIR="$ROOT/scripts/firecracker-snapshot-compat/infra"
TFVARS="${TF_DIR}/compat.auto.tfvars.json"
TFSTATE="${TF_DIR}/terraform.tfstate"

RESOURCE_GROUP="${RESOURCE_GROUP:-}"
DEPLOYMENT_NAME="${DEPLOYMENT_NAME:-}"
FORCE=0
RESET_LOCAL_STATE=1

usage() {
	cat >&2 <<EOF
Usage: $0 [--deployment NAME] [--resource-group RG] [--force] [--keep-local-state]

Deletes snapshot-compat Azure resources by deployment name prefix.
Safe to use when Terraform state/config no longer matches what was provisioned.

Examples:
  RESOURCE_GROUP=spokes-gwc DEPLOYMENT_NAME=fc-snap-compat-1781245099 $0
  $0 --resource-group spokes-gwc --deployment fc-snap-compat-1781245099 --force
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--resource-group | -g)
		RESOURCE_GROUP="$2"
		shift 2
		;;
	--deployment | -d)
		DEPLOYMENT_NAME="$2"
		shift 2
		;;
	--force | -f)
		FORCE=1
		shift
		;;
	--keep-local-state)
		RESET_LOCAL_STATE=0
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage
		exit 1
		;;
	esac
done

read_json_field() {
	local file=$1 field=$2
	node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); const v=j[process.argv[2]]; if(v!==undefined&&v!==null) process.stdout.write(String(v));' "$file" "$field"
}

read_tfstate_output() {
	local field=$1
	node -e '
const fs=require("fs");
const field=process.argv[1];
const state=JSON.parse(fs.readFileSync(process.argv[2],"utf8"));
const value=state.outputs?.[field]?.value;
if(value!==undefined&&value!==null) process.stdout.write(String(value));
' "$field" "$TFSTATE"
}

if [[ -z "$RESOURCE_GROUP" && -f "$TFVARS" ]]; then
	RESOURCE_GROUP="$(read_json_field "$TFVARS" resource_group_name)"
fi
if [[ -z "$DEPLOYMENT_NAME" && -f "$TFVARS" ]]; then
	DEPLOYMENT_NAME="$(read_json_field "$TFVARS" deployment_name)"
	if [[ -z "$DEPLOYMENT_NAME" ]]; then
		DEPLOYMENT_NAME="$(read_json_field "$TFVARS" vmss_name)"
	fi
fi
if [[ -z "$RESOURCE_GROUP" && -f "$TFSTATE" ]]; then
	RESOURCE_GROUP="$(node -e '
const fs=require("fs");
const state=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));
for (const r of state.resources ?? []) {
  if (r.type==="data.azurerm_resource_group" && r.instances?.[0]?.attributes?.name) {
    process.stdout.write(r.instances[0].attributes.name);
    break;
  }
}
' "$TFSTATE")"
fi
if [[ -z "$DEPLOYMENT_NAME" && -f "$TFSTATE" ]]; then
	DEPLOYMENT_NAME="$(read_tfstate_output deployment_name)"
	if [[ -z "$DEPLOYMENT_NAME" ]]; then
		DEPLOYMENT_NAME="$(read_tfstate_output vmss_name)"
	fi
fi

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required (or present in compat.auto.tfvars.json)}"
: "${DEPLOYMENT_NAME:?DEPLOYMENT_NAME is required (or present in compat.auto.tfvars.json / terraform.tfstate)}"

if ! command -v az >/dev/null 2>&1; then
	echo "ERROR: Azure CLI (az) is required" >&2
	exit 1
fi

az_resource_exists() {
	local type=$1 name=$2
	az resource show --resource-group "$RESOURCE_GROUP" --resource-type "$type" --name "$name" >/dev/null 2>&1
}

delete_if_exists() {
	local type=$1 name=$2
	if az_resource_exists "$type" "$name"; then
		echo "==> Deleting ${type}/${name}"
		az resource delete --resource-group "$RESOURCE_GROUP" --resource-type "$type" --name "$name" --verbose
	else
		echo "==> Skipping missing ${type}/${name}"
	fi
}

wait_for_vmss_delete() {
	local name=$1
	for _ in $(seq 1 60); do
		if ! az_resource_exists "Microsoft.Compute/virtualMachineScaleSets" "$name"; then
			return 0
		fi
		echo "==> Waiting for VMSS ${name} to finish deleting..."
		sleep 10
	done
	echo "ERROR: timed out waiting for VMSS ${name} to delete" >&2
	return 1
}

wait_for_vms_gone() {
	local prefix=$1
	for _ in $(seq 1 60); do
		local remaining
		remaining="$(az vm list -g "$RESOURCE_GROUP" --query "[?starts_with(name, '${prefix}')].name" -o tsv 2>/dev/null || true)"
		if [[ -z "$remaining" ]]; then
			return 0
		fi
		echo "==> Waiting for VMs to finish deleting: ${remaining}"
		sleep 10
	done
	echo "ERROR: timed out waiting for VMs with prefix ${prefix} to delete" >&2
	return 1
}

load_vm_names() {
	VM_NAMES=()
	while IFS= read -r name; do
		[[ -n "$name" ]] && VM_NAMES+=("$name")
	done < <(az vm list -g "$RESOURCE_GROUP" --query "[?starts_with(name, '${DEPLOYMENT_NAME}-')].name" -o tsv | sort)
}

LEGACY_VMSS=0
MULTI_VM=0
if az_resource_exists "Microsoft.Compute/virtualMachineScaleSets" "$DEPLOYMENT_NAME"; then
	LEGACY_VMSS=1
fi
if az vm show -g "$RESOURCE_GROUP" -n "${DEPLOYMENT_NAME}-0" >/dev/null 2>&1; then
	MULTI_VM=1
fi

PLANNED_DELETES=()
plan_delete() {
	PLANNED_DELETES+=("$1")
}

if [[ "$LEGACY_VMSS" -eq 1 ]]; then
	plan_delete "VMSS ${DEPLOYMENT_NAME}"
	plan_delete "load balancer ${DEPLOYMENT_NAME}-lb"
	plan_delete "public IP ${DEPLOYMENT_NAME}-pip"
elif [[ "$MULTI_VM" -eq 1 ]]; then
	load_vm_names
	for vm_name in "${VM_NAMES[@]}"; do
		[[ -n "$vm_name" ]] || continue
		plan_delete "VM ${vm_name}"
		plan_delete "NIC ${vm_name}-nic"
		plan_delete "public IP ${vm_name}-pip"
	done
fi
plan_delete "virtual network ${DEPLOYMENT_NAME}-vnet"
plan_delete "network security group ${DEPLOYMENT_NAME}-nsg"
if [[ "$RESET_LOCAL_STATE" -eq 1 ]]; then
	plan_delete "local Terraform state (${TF_DIR})"
fi

echo "Deployment: ${DEPLOYMENT_NAME}"
echo "Resource group: ${RESOURCE_GROUP}"
echo
echo "The following will be deleted:"
for item in "${PLANNED_DELETES[@]}"; do
	echo "  - ${item}"
done
echo

if [[ "$FORCE" -ne 1 ]]; then
	read -r -p "Delete these resources? [y/N] " answer
	case "$answer" in
	y | Y | yes | YES) ;;
	*)
		echo "Aborted."
		exit 1
		;;
	esac
fi

if [[ "$LEGACY_VMSS" -eq 1 ]]; then
	echo "==> Detected legacy VMSS deployment"
	delete_if_exists "Microsoft.Compute/virtualMachineScaleSets" "$DEPLOYMENT_NAME"
	wait_for_vmss_delete "$DEPLOYMENT_NAME"
	delete_if_exists "Microsoft.Network/loadBalancers" "${DEPLOYMENT_NAME}-lb"
	delete_if_exists "Microsoft.Network/publicIPAddresses" "${DEPLOYMENT_NAME}-pip"
elif [[ "$MULTI_VM" -eq 1 ]]; then
	echo "==> Detected multi-VM deployment"
	load_vm_names
	for vm_name in "${VM_NAMES[@]}"; do
		[[ -n "$vm_name" ]] || continue
		echo "==> Deleting VM ${vm_name}"
		az vm delete -g "$RESOURCE_GROUP" -n "$vm_name" --yes --no-wait
	done
	wait_for_vms_gone "${DEPLOYMENT_NAME}-"
	for vm_name in "${VM_NAMES[@]}"; do
		[[ -n "$vm_name" ]] || continue
		nic_name="${vm_name}-nic"
		pip_name="${vm_name}-pip"
		delete_if_exists "Microsoft.Network/networkInterfaces" "$nic_name"
		delete_if_exists "Microsoft.Network/publicIPAddresses" "$pip_name"
	done
else
	echo "WARN: no VMSS or ${DEPLOYMENT_NAME}-0 VM found; continuing with shared network cleanup"
fi

if az network vnet subnet show -g "$RESOURCE_GROUP" --vnet-name "${DEPLOYMENT_NAME}-vnet" -n "${DEPLOYMENT_NAME}-subnet" >/dev/null 2>&1; then
	echo "==> Removing NSG association from subnet ${DEPLOYMENT_NAME}-subnet"
	az network vnet subnet update \
		-g "$RESOURCE_GROUP" \
		--vnet-name "${DEPLOYMENT_NAME}-vnet" \
		-n "${DEPLOYMENT_NAME}-subnet" \
		--remove networkSecurityGroup >/dev/null 2>&1 || true
fi

delete_if_exists "Microsoft.Network/virtualNetworks" "${DEPLOYMENT_NAME}-vnet"
delete_if_exists "Microsoft.Network/networkSecurityGroups" "${DEPLOYMENT_NAME}-nsg"

if [[ "$RESET_LOCAL_STATE" -eq 1 ]]; then
	rm -f "$TFVARS" "$TFSTATE" "${TFSTATE}.backup"
	echo "==> Removed local Terraform state and compat.auto.tfvars.json"
fi

echo "==> Deployment ${DEPLOYMENT_NAME} cleanup complete"
