#!/usr/bin/env bash
# Runs snapshot create/restore compatibility matrix across cluster VMs.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${OUT_DIR}/instances.json"
STUDY_PATHS_LIB="${ROOT}/scripts/firecracker-snapshot-compat/lib/study-paths.js"
MATRIX_MODE="${COMPAT_MATRIX_MODE:-smart}"
MATRIX_RESET="${COMPAT_MATRIX_RESET:-0}"
MATRIX_RESUME="${COMPAT_MATRIX_RESUME:-1}"
MATRIX_RESULTS_LIB="${ROOT}/scripts/firecracker-snapshot-compat/lib/matrix-results.js"
REMOTE_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"
# -n is ssh-only (prevents ssh from consuming the matrix plan on stdin); not valid for scp.
SSH_OPTS="-n ${REMOTE_OPTS}"
SCP_OPTS="${REMOTE_OPTS}"

if [[ ! -f "$INSTANCES_JSON" ]]; then
	echo "ERROR: ${INSTANCES_JSON} not found; run infra/provision-vmss.sh first" >&2
	exit 1
fi

RUN_ID="$(node "$STUDY_PATHS_LIB" run-id "$INSTANCES_JSON")"
STUDY_DIR="$(node "$STUDY_PATHS_LIB" study-dir "$OUT_DIR" "$RUN_ID")"
RESULTS_FILE="${STUDY_DIR}/results.jsonl"
MANIFEST_FILE="${STUDY_DIR}/run-manifest.json"
PLAN_FILE="${STUDY_DIR}/matrix-plan.json"
PLAN_TSV="${STUDY_DIR}/matrix-plan.tsv"

if [[ ! -f "$MANIFEST_FILE" ]]; then
	bash "${ROOT}/scripts/firecracker-snapshot-compat/analyze-fingerprints.sh"
fi

SSH_KEY="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.ssh_key_path)' "$INSTANCES_JSON")"
ADMIN="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.admin_username)' "$INSTANCES_JSON")"
INSTANCE_COUNT="$(node -e 'const j=require(process.argv[1]); process.stdout.write(String(j.instances.length))' "$INSTANCES_JSON")"

instance_ip() {
	node -e 'const j=require(process.argv[1]); const i=Number(process.argv[2]); process.stdout.write(j.instances.find((x)=>x.index===i)?.ip||j.instances[i]?.ip||"")' "$INSTANCES_JSON" "$1"
}

instance_meta() {
	node -e 'const j=require(process.argv[1]); const i=Number(process.argv[2]); const inst=j.instances.find((x)=>x.index===i)||j.instances[i]; process.stdout.write(JSON.stringify(inst||{}))' "$INSTANCES_JSON" "$1"
}

ssh_instance() {
	local index=$1
	shift
	ssh $SSH_OPTS -i "$SSH_KEY" "${ADMIN}@$(instance_ip "$index")" "$@"
}

declare -a MATRIX_ROOTS=()

matrix_root() {
	echo "${MATRIX_ROOTS[$1]}"
}

ensure_matrix_owner() {
	local index=$1
	ssh_instance "$index" "sudo chown -R \"\$(id -un):\$(id -gn)\" '$(matrix_root "$index")'"
}

snap_dir_for() {
	local creator=$1 variant=$2
	echo "$(matrix_root "$creator")/create-c${creator}-${variant}"
}

restore_dir_for() {
	local creator=$1 restorer=$2 variant=$3
	if [[ "$creator" -eq "$restorer" ]]; then
		snap_dir_for "$creator" "$variant"
		return
	fi
	echo "$(matrix_root "$restorer")/restore-c${creator}-r${restorer}-${variant}"
}

json_escape() {
	node -e 'process.stdout.write(JSON.stringify(process.argv[1] ?? ""))' "$1"
}

MATRIX_STEP=0
PLAN_TOTAL=0
PLAN_CREATE_TOTAL=0
PLAN_RESTORE_TOTAL=0
MATRIX_START_TS=0

format_duration() {
	local total=${1:-0}
	local h=$((total / 3600))
	local m=$(((total % 3600) / 60))
	local s=$((total % 60))
	if [[ "$h" -gt 0 ]]; then
		printf '%dh%02dm%02ds' "$h" "$m" "$s"
	elif [[ "$m" -gt 0 ]]; then
		printf '%dm%02ds' "$m" "$s"
	else
		printf '%ds' "$s"
	fi
}

matrix_result_exists() {
	local variant=$1 creator=$2 restorer=$3
	node "$MATRIX_RESULTS_LIB" has "$RESULTS_FILE" "$RUN_ID" "$MATRIX_MODE" "$variant" "$creator" "$restorer"
}

creator_create_recorded_ok() {
	local variant=$1 creator=$2
	node "$MATRIX_RESULTS_LIB" create-ok "$RESULTS_FILE" "$RUN_ID" "$MATRIX_MODE" "$variant" "$creator"
}

remote_snapshot_ready() {
	local creator=$1 variant=$2 snap_dir
	snap_dir="$(snap_dir_for "$creator" "$variant")"
	ssh_instance "$creator" "test -s '${snap_dir}/snapshot_state' && test -s '${snap_dir}/snapshot_mem'"
}

report_matrix_progress() {
	local label=$1
	local now elapsed pct eta_secs
	MATRIX_STEP=$((MATRIX_STEP + 1))
	now=$(date +%s)
	elapsed=$((now - MATRIX_START_TS))
	pct=0
	eta_secs=0
	if [[ "$PLAN_TOTAL" -gt 0 ]]; then
		pct=$((MATRIX_STEP * 100 / PLAN_TOTAL))
	fi
	if [[ "$MATRIX_STEP" -gt 0 && "$MATRIX_STEP" -lt "$PLAN_TOTAL" ]]; then
		eta_secs=$((elapsed * (PLAN_TOTAL - MATRIX_STEP) / MATRIX_STEP))
	fi
	echo "" >&2
	echo "==> Progress ${MATRIX_STEP}/${PLAN_TOTAL} (${pct}%) | elapsed $(format_duration "$elapsed") | ETA ~$(format_duration "$eta_secs")" >&2
	echo "    ${label}" >&2
}

# Classify failures: systemic, snapshot (bad create), portability (expected), other.
FAIL_FAST_SYSTEMIC="${COMPAT_FAIL_FAST_SYSTEMIC:-1}"
CONSECUTIVE_SYSTEMIC_FAILURES=0
LAST_SYSTEMIC_SIGNATURE=""
CLASSIFY_FAILURE="${ROOT}/scripts/firecracker-snapshot-compat/lib/classify-failure.js"

classify_failure() {
	local phase=$1 output=$2 creator=${3:-} restorer=${4:-}
	node "$CLASSIFY_FAILURE" "$phase" "$output" "$creator" "$restorer"
}

report_matrix_failure() {
	local phase=$1 variant=$2 creator=$3 restorer=$4 status=$5 output=$6
	local kind summary signature restorer_label
	IFS=$'\t' read -r kind summary <<< "$(classify_failure "$phase" "$output" "$creator" "$restorer")"
	if [[ -n "$restorer" ]]; then
		restorer_label=" -> restorer ${restorer}"
	else
		restorer_label=""
	fi

	case "$kind" in
	portability)
		echo "==> portability fail (expected): ${phase} variant=${variant} creator=${creator}${restorer_label}: ${summary}" >&2
		return 0
		;;
	snapshot | snapshot-same-host)
		echo "" >&2
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
		echo "!! MATRIX SNAPSHOT FAIL (bad create): ${phase} variant=${variant} creator=${creator}${restorer_label}" >&2
		echo "!! ${summary}" >&2
		if [[ "$kind" == "snapshot-same-host" ]]; then
			echo "!! same-host restore failed — snapshot at create time is invalid, not CPU portability" >&2
		else
			echo "!! truncated/invalid snapshot files — check create on creator ${creator}" >&2
		fi
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
		echo "" >&2
		return 0
		;;
	systemic)
		echo "" >&2
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
		echo "!! MATRIX SYSTEMIC FAIL: ${phase} variant=${variant} creator=${creator}${restorer_label}" >&2
		echo "!! ${summary}" >&2
		echo "!! (script/infra bug — not a CPU portability result)" >&2
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
		if [[ ${#output} -le 1200 ]]; then
			echo "$output" >&2
		else
			printf '%s\n' "$output" | head -c 600 >&2
			echo "...(truncated)..." >&2
			printf '%s\n' "$output" | tail -c 600 >&2
		fi
		echo "" >&2
		;;
	*)
		echo "==> WARN: ${phase} fail variant=${variant} creator=${creator}${restorer_label}: ${summary}" >&2
		;;
	esac
}

maybe_abort_on_bad_failure() {
	local phase=$1 output=$2 creator=${3:-} restorer=${4:-}
	local kind summary signature
	[[ "$FAIL_FAST_SYSTEMIC" == "1" ]] || return 0
	IFS=$'\t' read -r kind summary <<< "$(classify_failure "$phase" "$output" "$creator" "$restorer")"
	[[ "$kind" == "systemic" || "$kind" == "snapshot-same-host" ]] || return 0
	signature="${kind}:${phase}:${summary}"
	if [[ "$signature" == "$LAST_SYSTEMIC_SIGNATURE" ]]; then
		CONSECUTIVE_SYSTEMIC_FAILURES=$((CONSECUTIVE_SYSTEMIC_FAILURES + 1))
	else
		LAST_SYSTEMIC_SIGNATURE="$signature"
		CONSECUTIVE_SYSTEMIC_FAILURES=1
	fi
	if [[ "$CONSECUTIVE_SYSTEMIC_FAILURES" -ge 2 ]]; then
		echo "ERROR: aborting matrix after ${CONSECUTIVE_SYSTEMIC_FAILURES} consecutive ${kind} failures." >&2
		if [[ "$kind" == "snapshot-same-host" ]]; then
			echo "       Snapshot create produced invalid output on the same host — not a portability result." >&2
		else
			echo "       This usually means a script or Firecracker API ordering bug—not a portability result." >&2
		fi
		echo "       Signature: ${signature}" >&2
		echo "       Set COMPAT_FAIL_FAST_SYSTEMIC=0 to run the full matrix anyway." >&2
		exit 1
	fi
}

verify_snapshot_files() {
	local creator=$1 variant=$2
	local snap_dir verify_output
	snap_dir="$(snap_dir_for "$creator" "$variant")"
	if verify_output="$(ssh_instance "$creator" \
		"d='${snap_dir}'; st=\$(wc -c <\"\${d}/snapshot_state\" 2>/dev/null || echo 0); mem=\$(wc -c <\"\${d}/snapshot_mem\" 2>/dev/null || echo 0); if [[ ! -s \"\${d}/snapshot_state\" || ! -s \"\${d}/snapshot_mem\" ]]; then echo \"ERROR: snapshot files missing or empty in \${d} (state=\${st} mem=\${mem})\"; exit 1; fi" 2>&1)"; then
		return 0
	fi
	report_matrix_failure "create-verify" "$variant" "$creator" "$creator" "fail" "$verify_output"
	maybe_abort_on_bad_failure "create-verify" "$verify_output" "$creator" "$creator"
	return 1
}

append_result() {
	local variant=$1 creator=$2 restorer=$3 status=$4 message=$5 portability_test=${6:-}
	local message_file
	mkdir -p "$STUDY_DIR"
	message_file="$(mktemp)"
	printf '%s' "$message" >"$message_file"
	node "${ROOT}/scripts/firecracker-snapshot-compat/lib/append-matrix-result.js" \
		"$RESULTS_FILE" \
		"$INSTANCES_JSON" \
		"$RUN_ID" \
		"$MATRIX_MODE" \
		"$variant" \
		"$creator" \
		"$restorer" \
		"$status" \
		"${portability_test:-}" \
		"$message_file"
	rm -f "$message_file"
}

run_restore() {
	local variant=$1 creator=$2 restorer=$3 portability_test=$4
	local restore_dir
	restore_dir="$(restore_dir_for "$creator" "$restorer" "$variant")"
	if [[ "$creator" -ne "$restorer" ]]; then
		tmp_mem="$(mktemp)"
		tmp_state="$(mktemp)"
		ssh_instance "$restorer" "mkdir -p '${restore_dir}'"
		scp $SCP_OPTS -i "$SSH_KEY" \
			"${ADMIN}@$(instance_ip "$creator"):$(snap_dir_for "$creator" "$variant")/snapshot_mem" "$tmp_mem"
		scp $SCP_OPTS -i "$SSH_KEY" \
			"${ADMIN}@$(instance_ip "$creator"):$(snap_dir_for "$creator" "$variant")/snapshot_state" "$tmp_state"
		scp $SCP_OPTS -i "$SSH_KEY" "$tmp_mem" \
			"${ADMIN}@$(instance_ip "$restorer"):${restore_dir}/snapshot_mem"
		scp $SCP_OPTS -i "$SSH_KEY" "$tmp_state" \
			"${ADMIN}@$(instance_ip "$restorer"):${restore_dir}/snapshot_state"
		rm -f "$tmp_mem" "$tmp_state"
	fi

	if output="$(ssh_instance "$restorer" \
		"sudo bash ~/project/scripts/firecracker-snapshot-compat/restore-snapshot.sh ${variant} ${restore_dir}" 2>&1)"; then
		CONSECUTIVE_SYSTEMIC_FAILURES=0
		LAST_SYSTEMIC_SIGNATURE=""
		append_result "$variant" "$creator" "$restorer" "pass" "$output" "$portability_test"
	else
		report_matrix_failure "restore" "$variant" "$creator" "$restorer" "fail" "$output"
		maybe_abort_on_bad_failure "restore" "$output" "$creator" "$restorer"
		append_result "$variant" "$creator" "$restorer" "fail" "$output" "$portability_test"
	fi
	ensure_matrix_owner "$restorer"
}

mkdir -p "$STUDY_DIR"
node -e 'const {updateLatestLink}=require(process.argv[1]); updateLatestLink(process.argv[2], process.argv[3]);' \
	"$STUDY_PATHS_LIB" "$OUT_DIR" "$RUN_ID"
echo "==> Study results dir: ${STUDY_DIR}"
if [[ "$MATRIX_RESET" == "1" ]]; then
	: >"$RESULTS_FILE"
	echo "==> Reset results file: ${RESULTS_FILE}"
elif [[ -s "$RESULTS_FILE" ]]; then
	existing_count=$(wc -l <"$RESULTS_FILE" | tr -d '[:space:]')
	echo "==> Resuming study (${existing_count} rows recorded): ${RESULTS_FILE}"
else
	: >"$RESULTS_FILE"
fi
if [[ "$MATRIX_RESUME" == "1" ]]; then
	echo "==> Resume enabled: skipping steps already recorded for study ${RUN_ID}"
else
	echo "==> Resume disabled: all plan steps will execute (results still append unless COMPAT_MATRIX_RESET=1)"
fi

node "${ROOT}/scripts/firecracker-snapshot-compat/lib/matrix-plan.js" \
	"$INSTANCES_JSON" "$MATRIX_MODE" >"$PLAN_FILE"
node "${ROOT}/scripts/firecracker-snapshot-compat/lib/matrix-plan.js" \
	"$INSTANCES_JSON" "$MATRIX_MODE" tsv >"$PLAN_TSV"
node -e 'const p=require(process.argv[1]); console.log(`==> Matrix mode: ${p.mode}`); console.log(`==> CPU signatures: ${p.distinct_cpu_signatures}`); console.log(`==> Planned creates: ${p.create_count} (full matrix would be ${p.full_matrix_create_count})`); console.log(`==> Planned restores: ${p.restore_count} (full matrix would be ${p.full_matrix_restore_count})`);' \
	"$PLAN_FILE"
PLAN_TOTAL=$(wc -l <"$PLAN_TSV" | tr -d '[:space:]')
read -r PLAN_CREATE_TOTAL PLAN_RESTORE_TOTAL <<<"$(node -e 'const p=require(process.argv[1]); process.stdout.write(`${p.create_count} ${p.restore_count}`)' "$PLAN_FILE")"

echo "==> Ensuring sandbox daemon is built on all instances..."
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	home_dir="$(ssh_instance "$index" 'printf %s "$HOME"')"
	MATRIX_ROOTS[$index]="${home_dir}/fc-compat-matrix"
	if [[ "$MATRIX_RESET" == "1" ]]; then
		ssh_instance "$index" \
			"sudo rm -rf '$(matrix_root "$index")' && mkdir -p '$(matrix_root "$index")' && cd ~/project && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sandbox-daemon ./cmd/daemon"
	else
		ssh_instance "$index" \
			"mkdir -p '$(matrix_root "$index")' && cd ~/project && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sandbox-daemon ./cmd/daemon"
	fi
done

echo "==> Syncing compat scripts to all instances..."
COMPAT_SCRIPTS_TAR="$(mktemp)"
GNUTAR=$(command -v gtar || command -v tar)
COPYFILE_DISABLE=1 "$GNUTAR" czf "$COMPAT_SCRIPTS_TAR" \
	--no-xattrs \
	--exclude='.DS_Store' \
	--exclude='._*' \
	-C "${ROOT}/scripts" firecracker-snapshot-compat
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ssh_instance "$index" "mkdir -p ~/project/scripts"
	scp $SCP_OPTS -i "$SSH_KEY" "$COMPAT_SCRIPTS_TAR" \
		"${ADMIN}@$(instance_ip "$index"):/tmp/fc-compat-scripts.tar.gz"
	ssh_instance "$index" "tar xzf /tmp/fc-compat-scripts.tar.gz -C ~/project/scripts && rm /tmp/fc-compat-scripts.tar.gz"
done
rm -f "$COMPAT_SCRIPTS_TAR"

echo "==> Syncing helper CPU configs to all instances..."
bash "${ROOT}/scripts/firecracker-snapshot-compat/sync-cpu-configs.sh" "$INSTANCES_JSON"

MATRIX_START_TS=$(date +%s)
echo "==> Starting matrix workload (${PLAN_CREATE_TOTAL} creates + ${PLAN_RESTORE_TOTAL} restores = ${PLAN_TOTAL} steps)..."

last_create_ok=1
while IFS=$'\t' read -r action variant creator restorer same_host portability_test; do
	if [[ "$action" == "create" ]]; then
		if [[ "$variant" != "${last_variant:-}" ]]; then
			echo "==> Variant ${variant}"
			last_variant="$variant"
		fi
		snap_dir="$(snap_dir_for "$creator" "$variant")"
		if [[ "$MATRIX_RESUME" == "1" ]] && { creator_create_recorded_ok "$variant" "$creator" || remote_snapshot_ready "$creator" "$variant"; }; then
			report_matrix_progress "create skipped (already done) variant=${variant} on instance ${creator}"
			last_create_ok=1
			continue
		fi
		report_matrix_progress "create variant=${variant} on instance ${creator}"
		if create_output="$(ssh_instance "$creator" \
			"bash ~/project/scripts/firecracker-snapshot-compat/create-snapshot.sh ${variant} ${snap_dir}" 2>&1)"; then
			echo "$create_output"
			if verify_snapshot_files "$creator" "$variant"; then
				last_create_ok=1
				CONSECUTIVE_SYSTEMIC_FAILURES=0
				LAST_SYSTEMIC_SIGNATURE=""
			else
				last_create_ok=0
			fi
			ensure_matrix_owner "$creator"
		else
			report_matrix_failure "create" "$variant" "$creator" "" "fail" "$create_output"
			maybe_abort_on_bad_failure "create" "$create_output" "$creator" "$creator"
			last_create_ok=0
		fi
		continue
	fi

	if [[ "$action" != "restore" ]]; then
		continue
	fi
	if [[ "$MATRIX_RESUME" == "1" ]] && matrix_result_exists "$variant" "$creator" "$restorer"; then
		report_matrix_progress "restore skipped (already recorded) variant=${variant} creator=${creator} -> restorer ${restorer}"
		continue
	fi
	if [[ "$last_create_ok" -ne 1 ]]; then
		report_matrix_progress "restore skipped (create failed) variant=${variant} creator=${creator} -> restorer ${restorer}"
		report_matrix_failure "restore-skipped" "$variant" "$creator" "$restorer" "create_failed" \
			"snapshot creation failed on creator ${creator}; skipping restore"
		append_result "$variant" "$creator" "$restorer" "create_failed" "snapshot creation failed on creator ${creator}" "$portability_test"
		continue
	fi
	report_matrix_progress "restore variant=${variant} creator=${creator} -> restorer ${restorer}"
	run_restore "$variant" "$creator" "$restorer" "$portability_test"
done <"$PLAN_TSV"

bash "$ROOT/scripts/firecracker-snapshot-compat/summarize-results.sh" "$STUDY_DIR"
node "${ROOT}/scripts/firecracker-snapshot-compat/lib/generate-intel-report.js" "$STUDY_DIR"
node - "$RESULTS_FILE" "$CLASSIFY_FAILURE" <<'NODE'
const fs = require('fs');
const resultsPath = process.argv[2];
const classifyPath = process.argv[3];
const { failureKindFromMessage } = require(classifyPath);

const rows = fs.readFileSync(resultsPath, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
const counts = rows.reduce((acc, row) => {
  acc[row.status] = (acc[row.status] || 0) + 1;
  return acc;
}, {});
const failRows = rows.filter((r) => r.status === 'fail');
const portabilityFails = failRows.filter((r) => failureKindFromMessage(r.message, r.creator_index, r.restorer_index) === 'portability').length;
const snapshotFails = failRows.filter((r) => {
  const k = failureKindFromMessage(r.message, r.creator_index, r.restorer_index);
  return k === 'snapshot' || k === 'snapshot-same-host';
}).length;
const unexpectedFails = failRows.length - portabilityFails - snapshotFails;
const parts = Object.entries(counts).sort().map(([k, v]) => `${k}=${v}`);
console.log(`==> Results: ${parts.join(', ') || 'none'}`);
if (portabilityFails > 0) {
  console.log(`==> Portability failures (expected): ${portabilityFails}`);
}
if (snapshotFails > 0) {
  console.error(`==> Snapshot/create failures (invalid artifacts): ${snapshotFails}`);
}
if (unexpectedFails > 0 || (counts.create_failed || 0) > 0) {
  console.error(`==> Other unexpected failures: ${unexpectedFails + (counts.create_failed || 0)} (see stderr above and summary.md)`);
  process.exit(1);
}
if (snapshotFails > 0) {
  process.exit(1);
}
NODE
total_elapsed=$(( $(date +%s) - MATRIX_START_TS ))
echo "==> Matrix complete in $(format_duration "$total_elapsed"): ${STUDY_DIR}"
