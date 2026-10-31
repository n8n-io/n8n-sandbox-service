#!/usr/bin/env bash
# Deletes the VMs, NICs, public IPs, NSGs, disks and VNets tagged with
# purpose=sandbox-service-e2e that are older than 1 hour, plus the OS disks of
# the VMs it deletes. Intended as a scheduled safety net for resources leaked by
# failed or cancelled e2e runs.
# Exits non-zero when a deletion fails.
# Requires: RESOURCE_GROUP env var and authenticated az CLI.
set -euo pipefail

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TAG="purpose=sandbox-service-e2e"
CUTOFF_EPOCH=$(
	python3 - <<'PY'
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

lower() {
	tr '[:upper:]' '[:lower:]' <<<"$1"
}

# Only the types e2e/infra creates, dependents first: VMs, then NICs, then what
# they reference, then VNets. Prints nothing for any other type.
delete_pass() {
	case "$(lower "$1")" in
	microsoft.compute/virtualmachines) echo 1 ;;
	microsoft.network/networkinterfaces) echo 2 ;;
	microsoft.network/publicipaddresses | microsoft.network/networksecuritygroups | microsoft.compute/disks) echo 3 ;;
	microsoft.network/virtualnetworks) echo 4 ;;
	esac
}

echo "==> Finding resources tagged ${TAG} in ${RESOURCE_GROUP}..."
RESOURCES=$(az resource list \
	--resource-group "$RESOURCE_GROUP" \
	--query "[?tags.purpose=='sandbox-service-e2e'].[id, type, createdTime]" \
	-o tsv)

STALE=()
while IFS=$'\t' read -r id type created; do
	[[ -n "$id" ]] || continue
	name="${id##*/}"
	pass=$(delete_pass "$type")
	if [[ -z "$pass" ]]; then
		echo "==> Skipping ${name}; unexpected type ${type}"
		continue
	fi
	if [[ -z "$created" ]]; then
		echo "==> Skipping ${name}; creation time is unavailable"
		continue
	fi
	created_epoch=$(iso_to_epoch "$created" 2>/dev/null) || {
		echo "==> Skipping ${name}; could not parse creation time: ${created}"
		continue
	}
	if [[ "$created_epoch" -gt "$CUTOFF_EPOCH" ]]; then
		echo "==> Skipping ${name}; less than 1 hour old"
		continue
	fi
	STALE+=("${pass} ${id}")
done <<<"$RESOURCES"

if [[ ${#STALE[@]} -eq 0 ]]; then
	echo "No stale e2e resources found. Nothing to clean up."
	exit 0
fi

echo "==> Deleting ${#STALE[@]} stale resource(s)..."

FAILED=0
DELETED=""

delete_resource() {
	local id="$1"
	if grep -Fxq "$(lower "$id")" <<<"$DELETED"; then
		return 0
	fi
	echo "==> Deleting ${id##*/}..."
	if az resource delete --ids "$id" -o none; then
		DELETED+="$(lower "$id")"$'\n'
	else
		echo "ERROR: could not delete ${id##*/}" >&2
		FAILED=$((FAILED + 1))
		return 1
	fi
}

for pass in 1 2 3 4; do
	for entry in "${STALE[@]}"; do
		[[ "${entry%% *}" == "$pass" ]] || continue
		id="${entry#* }"
		if [[ "$pass" != 1 ]]; then
			delete_resource "$id" || true
			continue
		fi
		# Deleting a VM leaves its OS disk behind, tagged or not. Only delete
		# it when it is in the same resource group.
		os_disk=$(az vm show --ids "$id" --query 'storageProfile.osDisk.managedDisk.id' -o tsv) || os_disk=""
		if [[ -n "$os_disk" && "$(lower "$os_disk")" != "$(lower "${id%%/providers/*}")/providers/microsoft.compute/disks/"* ]]; then
			echo "==> Not deleting OS disk ${os_disk##*/}; it is in another resource group"
			os_disk=""
		fi
		if delete_resource "$id" && [[ -n "$os_disk" ]]; then
			delete_resource "$os_disk" || true
		fi
	done
done

if [[ "$FAILED" -gt 0 ]]; then
	echo "==> Sweep finished with ${FAILED} failed deletion(s)" >&2
	exit 1
fi

echo "==> Sweep complete"
