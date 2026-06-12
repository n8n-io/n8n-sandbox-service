#!/usr/bin/env bash
# Provisions a throwaway cluster of VMs for Firecracker snapshot compatibility testing.
# Requires: RESOURCE_GROUP env var.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$ROOT"

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TF_DIR="scripts/firecracker-snapshot-compat/infra"
DEPLOYMENT_NAME="${COMPAT_DEPLOYMENT_NAME:-fc-snap-compat-${GITHUB_RUN_ID:-$(date +%s)}}"
LOCATION="${COMPAT_LOCATION:-germanywestcentral}"
VM_SIZE="${COMPAT_VM_SIZE:-Standard_D4s_v3}"
INSTANCE_COUNT="${COMPAT_INSTANCE_COUNT:-3}"
OS_DISK_SIZE_GB="${COMPAT_OS_DISK_SIZE_GB:-80}"
SSH_KEY_PATH="${COMPAT_SSH_KEY_PATH:-$HOME/.ssh/fc-snap-compat-key}"
VM_ADMIN="azureuser"
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"
SSH_WAIT_ATTEMPTS="${COMPAT_SSH_WAIT_ATTEMPTS:-60}"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
TF_STATE="${COMPAT_TF_STATE:-}"
FINGERPRINT_DIR="${COMPAT_FINGERPRINT_DIR:-}"
INSTANCES_JSON="${COMPAT_INSTANCES_JSON:-${OUT_DIR}/instances.json}"

default_cohort_id() {
	if [[ -n "${COMPAT_COHORT_ID:-}" ]]; then
		echo "${COMPAT_COHORT_ID}"
		return
	fi
	echo "${LOCATION}-${VM_SIZE}" | tr '[:upper:]' '[:lower:]' | sed 's/standard_//; s/_/-/g'
}
COHORT_ID="$(default_cohort_id)"
if [[ -z "$FINGERPRINT_DIR" ]]; then
	FINGERPRINT_DIR="${OUT_DIR}/fingerprints/${COHORT_ID}"
fi
mkdir -p "$FINGERPRINT_DIR"

shell_quote() {
	printf "%q" "$1"
}

mkdir -p "$(dirname "$SSH_KEY_PATH")"
if [[ ! -f "$SSH_KEY_PATH" ]]; then
	echo "==> Generating ephemeral SSH keypair..."
	ssh-keygen -t ed25519 -f "$SSH_KEY_PATH" -N "" -q
else
	echo "==> Using existing SSH keypair at ${SSH_KEY_PATH}"
fi

cat > "${TF_DIR}/compat.auto.tfvars.json" <<EOF
{
  "resource_group_name": "$RESOURCE_GROUP",
  "deployment_name": "$DEPLOYMENT_NAME",
  "location": "$LOCATION",
  "vm_size": "$VM_SIZE",
  "instance_count": $INSTANCE_COUNT,
  "os_disk_size_gb": $OS_DISK_SIZE_GB,
  "ssh_public_key_path": "${SSH_KEY_PATH}.pub"
}
EOF

tf() {
	local cmd=$1
	shift
	# -state is valid for apply/output/etc., not for init (heterogeneous cohorts use COMPAT_TF_STATE).
	if [[ -n "$TF_STATE" && "$cmd" != "init" ]]; then
		terraform -chdir="$TF_DIR" "$cmd" -state="$TF_STATE" "$@"
	else
		terraform -chdir="$TF_DIR" "$cmd" "$@"
	fi
}

echo "==> Provisioning ${INSTANCE_COUNT} VMs via Terraform (cohort ${COHORT_ID})..."
tf init -input=false
tf apply -auto-approve -input=false

INSTANCE_IPS=()
while IFS= read -r line; do
	[[ -n "$line" ]] && INSTANCE_IPS+=("$line")
done < <(tf output -json instance_public_ips | node -e 'JSON.parse(require("fs").readFileSync(0,"utf8")).forEach((ip)=>console.log(ip))')
INSTANCE_NAMES=()
while IFS= read -r line; do
	[[ -n "$line" ]] && INSTANCE_NAMES+=("$line")
done < <(tf output -json instance_names | node -e 'JSON.parse(require("fs").readFileSync(0,"utf8")).forEach((name)=>console.log(name))')
DEPLOYMENT_NAME_OUT="$(tf output -raw deployment_name)"
FINGERPRINT_REL_PREFIX="fingerprints/${COHORT_ID}"

instance_ip() {
	local index=$1
	echo "${INSTANCE_IPS[$index]}"
}

ssh_instance() {
	local index=$1
	shift
	ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@$(instance_ip "$index")" "$@"
}

wait_for_ssh() {
	local index=$1
	local ip
	ip="$(instance_ip "$index")"
	echo "==> Waiting for SSH on instance ${index} (${ip})..."
	for i in $(seq 1 "$SSH_WAIT_ATTEMPTS"); do
		if ssh $SSH_OPTS -o ConnectTimeout=5 -i "$SSH_KEY_PATH" "${VM_ADMIN}@${ip}" "echo ready" 2>/dev/null; then
			echo "Instance ${index} SSH ready"
			return 0
		fi
		if [[ "$i" -eq "$SSH_WAIT_ATTEMPTS" ]]; then
			echo "SSH connection failed for instance ${index} (${ip}) after $((SSH_WAIT_ATTEMPTS * 3)) seconds" >&2
			echo "Hint: verify NSG allows port 22 and the VM finished provisioning in Azure portal." >&2
			return 1
		fi
		sleep 3
	done
}

for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	wait_for_ssh "$index"
done

echo "==> Transferring code to VMs..."
GNUTAR=$(command -v gtar || command -v tar)
COPYFILE_DISABLE=1 "$GNUTAR" czf /tmp/fc-compat-repo.tar.gz \
	--no-xattrs \
	--exclude=.git \
	--exclude='.DS_Store' \
	--exclude='._*' \
	--exclude=bin \
	--exclude=dist \
	--exclude=node_modules \
	--exclude='e2e/infra/.terraform' \
	--exclude='e2e/infra/*.tfstate*' \
	--exclude='scripts/firecracker-snapshot-compat/infra/.terraform' \
	--exclude='scripts/firecracker-snapshot-compat/infra/*.tfstate*' \
	--exclude='scripts/firecracker-snapshot-compat/results' \
	--exclude='scripts/firecracker-snapshot-compat/fingerprints' \
	-C "$ROOT" .

for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	scp $SSH_OPTS -i "$SSH_KEY_PATH" /tmp/fc-compat-repo.tar.gz \
		"${VM_ADMIN}@$(instance_ip "$index"):/tmp/fc-compat-repo.tar.gz"
	ssh_instance "$index" \
		"mkdir -p ~/project && tar xzf /tmp/fc-compat-repo.tar.gz -C ~/project && rm /tmp/fc-compat-repo.tar.gz"
done
rm -f /tmp/fc-compat-repo.tar.gz

echo "==> Setting up Firecracker on each VM..."
REMOTE_ENV=""
for var in FIRECRACKER_VERSION FIRECRACKER_TARBALL_SHA256 FIRECRACKER_CI_VERSION FIRECRACKER_E2E_ROOTFS_SIZE_MB; do
	if [[ -n "${!var:-}" ]]; then
		REMOTE_ENV+=" ${var}=$(shell_quote "${!var}")"
	fi
done

for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	echo "==> Setup instance ${index} ($(instance_ip "$index"))..."
	ssh_instance "$index" \
		"${REMOTE_ENV:+${REMOTE_ENV} }FIRECRACKER_E2E_SKIP_GOLDEN_SNAPSHOT=1 bash ~/project/e2e/infra/scripts/setup-firecracker-e2e-vm.sh"
done

echo "==> Collecting host fingerprints..."
instances_json='{"deployment_name":"'"$DEPLOYMENT_NAME_OUT"'","cohort_id":"'"$COHORT_ID"'","location":"'"$LOCATION"'","vm_size":"'"$VM_SIZE"'","resource_group":"'"$RESOURCE_GROUP"'","ssh_key_path":"'"$SSH_KEY_PATH"'","admin_username":"'"$VM_ADMIN"'","instances":['
first=1
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	instance_id="$(ssh_instance "$index" "curl -fsSL -H 'Metadata: true' 'http://169.254.169.254/metadata/instance/compute/name?api-version=2021-02-01&format=text' 2>/dev/null || hostname")"
	fingerprint_path="${FINGERPRINT_DIR}/instance-${index}.json"
	ssh_instance "$index" "bash ~/project/scripts/firecracker-snapshot-compat/collect-host-fingerprint.sh" >"$fingerprint_path"
	if [[ "$first" -eq 1 ]]; then
		first=0
	else
		instances_json+=','
	fi
	instances_json+='{"index":'"$index"',"name":"'"$instance_id"'","ip":"'"${INSTANCE_IPS[$index]}"'","cohort_id":"'"$COHORT_ID"'","fingerprint_file":"'"${FINGERPRINT_REL_PREFIX}"'/instance-'"$index"'.json"}'
done
instances_json+=']}'
printf '%s\n' "$instances_json" >"$INSTANCES_JSON"

COMPAT_OUT_DIR="$OUT_DIR" bash "${ROOT}/scripts/firecracker-snapshot-compat/analyze-fingerprints.sh"

echo "==> Syncing fingerprints to VMs..."
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ssh_instance "$index" "mkdir -p ~/project/scripts/firecracker-snapshot-compat/fingerprints/${COHORT_ID}"
	for fp in "${FINGERPRINT_DIR}"/*.json; do
		[[ -f "$fp" ]] || continue
		scp $SSH_OPTS -i "$SSH_KEY_PATH" "$fp" \
			"${VM_ADMIN}@$(instance_ip "$index"):~/project/scripts/firecracker-snapshot-compat/fingerprints/${COHORT_ID}/$(basename "$fp")"
	done
done

HELPER_CONFIG="${OUT_DIR}/cpu-configs/helper-custom.json"
HELPER_INTEL_CONFIG="${OUT_DIR}/cpu-configs/helper-intel-only.json"
mkdir -p "${OUT_DIR}/cpu-configs"
bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-custom-config.sh" \
	--out "$HELPER_CONFIG" \
	--compat-dir "$OUT_DIR" \
	"${FINGERPRINT_DIR}"/*.json
COMPAT_INSTANCES_JSON="${COMPAT_INSTANCES_JSON:-$INSTANCES_JSON}" \
	bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-intel-config.sh" \
	--out "$HELPER_INTEL_CONFIG"
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ssh_instance "$index" "mkdir -p /tmp/fc-compat-cpu-configs"
	scp $SSH_OPTS -i "$SSH_KEY_PATH" "$HELPER_CONFIG" \
		"${VM_ADMIN}@$(instance_ip "$index"):/tmp/fc-compat-cpu-configs/helper-custom.json"
	if [[ -f "$HELPER_INTEL_CONFIG" ]]; then
		scp $SSH_OPTS -i "$SSH_KEY_PATH" "$HELPER_INTEL_CONFIG" \
			"${VM_ADMIN}@$(instance_ip "$index"):/tmp/fc-compat-cpu-configs/helper-intel-only.json"
	fi
	ssh_instance "$index" "printf '%s\n' '{\"kvm_capabilities\": [\"!56\"]}' > /tmp/fc-compat-cpu-configs/no-xcrs.json"
done

STUDY_RUN_ID="$(node "${ROOT}/scripts/firecracker-snapshot-compat/lib/study-paths.js" run-id "${INSTANCES_JSON}" 2>/dev/null || true)"
if [[ -n "$STUDY_RUN_ID" && -f "${OUT_DIR}/results/${STUDY_RUN_ID}/run-manifest.json" ]]; then
	node -e 'const m=require(process.argv[1]); console.log(m.interpretation)' "${OUT_DIR}/results/${STUDY_RUN_ID}/run-manifest.json"
fi
echo "==> Cluster ${DEPLOYMENT_NAME_OUT} ready. Instance metadata: ${INSTANCES_JSON}"
