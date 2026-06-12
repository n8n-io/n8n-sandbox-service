#!/usr/bin/env bash
# Summarizes snapshot compatibility matrix results as Markdown.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
STUDY_PATHS_LIB="${SCRIPT_DIR}/lib/study-paths.js"
MATRIX_RESULTS_LIB="${SCRIPT_DIR}/lib/matrix-results.js"

resolve_study_dir() {
	if [[ -n "${1:-}" ]]; then
		if [[ -d "$1" ]]; then
			printf '%s' "$1"
			return
		fi
		if [[ -f "$1" ]]; then
			dirname "$1"
			return
		fi
		echo "ERROR: not a study directory or results file: $1" >&2
		exit 1
	fi
	local resolved
	if ! resolved="$(node "$STUDY_PATHS_LIB" resolve "$OUT_DIR" "${OUT_DIR}/instances.json")"; then
		echo "ERROR: could not resolve study directory; pass a study path or ensure instances.json exists" >&2
		exit 1
	fi
	printf '%s' "$resolved"
}

STUDY_DIR="$(resolve_study_dir "${1:-}")"
RESULTS_FILE="${STUDY_DIR}/results.jsonl"
OUT_FILE="${STUDY_DIR}/summary.md"
MANIFEST_FILE="${STUDY_DIR}/run-manifest.json"

if [[ ! -f "$RESULTS_FILE" ]]; then
	echo "ERROR: results file not found: ${RESULTS_FILE}" >&2
	exit 1
fi

node - "$RESULTS_FILE" "$OUT_FILE" "$MANIFEST_FILE" "$MATRIX_RESULTS_LIB" <<'NODE'
const fs = require('fs');
const { latestRows } = require(process.argv[5]);
const [,, resultsPath, outPath, manifestPath] = process.argv;
const lines = fs.readFileSync(resultsPath, 'utf8').trim().split('\n').filter(Boolean);
const allRows = lines.map((line) => JSON.parse(line));
const manifest = fs.existsSync(manifestPath) ? JSON.parse(fs.readFileSync(manifestPath, 'utf8')) : null;
const activeRunId = manifest?.study_id || manifest?.run_id || null;
let tableRows = activeRunId ? latestRows(allRows, { runId: activeRunId }) : latestRows(allRows);
if (activeRunId && tableRows.length === 0 && allRows.length > 0) {
  tableRows = latestRows(allRows);
}

const variants = [...new Set(tableRows.map((r) => r.variant))];
const creators = [...new Set(tableRows.map((r) => r.creator_index))].sort((a, b) => a - b);
const restorers = [...new Set(tableRows.map((r) => r.restorer_index))].sort((a, b) => a - b);

let md = '# Firecracker snapshot compatibility summary\n\n';
md += `Generated: ${new Date().toISOString()}\n\n`;

if (manifest) {
  md += '## Run context\n\n';
  md += `- **Run ID:** ${manifest.run_id}\n`;
  if (manifest.deployment_name) md += `- **Deployment:** ${manifest.deployment_name}\n`;
  if (manifest.cohort_id) md += `- **Cohort:** ${manifest.cohort_id}\n`;
  if (manifest.location) md += `- **Location:** ${manifest.location}\n`;
  if (manifest.vm_size) md += `- **VM size:** ${manifest.vm_size}\n`;
  md += `- **CPU homogeneous:** ${manifest.cpu_homogeneous ? 'yes' : 'no'}\n`;
  md += `- **Distinct CPU signatures:** ${manifest.distinct_cpu_count}\n`;
  md += `- **Cross-host portability meaningful:** ${manifest.cross_host_portability_meaningful ? 'yes' : 'no'}\n`;
  md += `- **Summary cells:** ${tableRows.length}\n`;
  md += `\n${manifest.interpretation}\n\n`;
}

for (const variant of variants) {
  md += `## ${variant}\n\n`;
  md += '| creator \\\\ restorer |';
  for (const restorer of restorers) md += ` ${restorer} |`;
  md += '\n|';
  md += restorers.map(() => '---').join('|');
  md += '|\n';
  for (const creator of creators) {
    md += `| ${creator} |`;
    for (const restorer of restorers) {
      const row = tableRows.find((r) => r.variant === variant && r.creator_index === creator && r.restorer_index === restorer);
      if (!row) {
        md += ' n/a |';
        continue;
      }
      const label = row.cross_host_portability_test === false && creator !== restorer ? `${row.status}*` : row.status;
      md += ` ${label} |`;
    }
    md += '\n';
  }
  md += '\n';
}

md += '`*` = same CPU signature; cross-host cell is not a portability test.\n\n';

const portabilityRows = tableRows.filter((r) => r.cross_host_portability_test === true || r.cross_host_portability_test === 'true');
const sameCpuCrossHost = tableRows.filter((r) => (r.cross_host_portability_test === false || r.cross_host_portability_test === 'false') && r.creator_index !== r.restorer_index);
if (sameCpuCrossHost.length) {
  md += '## Same-CPU cross-host cells (informational only)\n\n';
  for (const row of sameCpuCrossHost) {
    md += `- **${row.variant}** creator ${row.creator_index} -> restorer ${row.restorer_index}: ${row.status}\n`;
  }
  md += '\n';
}

const failures = portabilityRows.filter((r) => r.status !== 'pass');
if (failures.length) {
  md += '## Portability failures\n\n';
  for (const row of failures) {
    md += `- **${row.variant}** creator ${row.creator_index} -> restorer ${row.restorer_index}: ${row.status}\n`;
    if (row.message) {
      md += `  \n  \`${String(row.message).replace(/\n/g, ' ').slice(0, 500)}\`\n`;
    }
  }
}

fs.writeFileSync(outPath, md);
console.log(`Wrote ${outPath}`);
NODE
