#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { latestRows } = require('./matrix-results');

const studyDir = path.resolve(process.argv[2] || '');
if (!studyDir) {
	console.error('usage: generate-intel-report.js <study-dir>');
	process.exit(2);
}

function findRepoRoot(fromDir) {
	let dir = path.resolve(fromDir);
	for (;;) {
		if (fs.existsSync(path.join(dir, 'go.mod'))) {
			return dir;
		}
		const parent = path.dirname(dir);
		if (parent === dir) {
			throw new Error(`repo root not found from ${fromDir}`);
		}
		dir = parent;
	}
}

const repoRoot = findRepoRoot(__dirname);
const resultsPath = path.join(studyDir, 'results.jsonl');
const manifestPath = path.join(studyDir, 'run-manifest.json');
const outPath = path.join(studyDir, 'REPORT.md');
const docsOutPath = path.join(repoRoot, 'docs/firecracker-intel-snapshot-compat-report.md');

const allRows = fs
	.readFileSync(resultsPath, 'utf8')
	.trim()
	.split('\n')
	.filter(Boolean)
	.map((line) => JSON.parse(line));
const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
const rows = latestRows(allRows, { runId: manifest.study_id });

function sameHostFails(variant) {
	return rows.filter(
		(r) => r.variant === variant && r.creator_index === r.restorer_index && r.status === 'fail',
	).length;
}

function variantPortStats(variant) {
	const port = rows.filter(
		(r) =>
			(r.cross_host_portability_test === true || r.cross_host_portability_test === 'true') &&
			r.variant === variant,
	);
	return {
		pass: port.filter((r) => r.status === 'pass').length,
		fail: port.filter((r) => r.status === 'fail').length,
		sameHostFail: sameHostFails(variant),
	};
}

function variantCreateFailed(variant) {
	return rows.filter((r) => r.variant === variant && r.status === 'create_failed').length;
}

const t2s = variantPortStats('T2S');
const helperCreateFailed = variantCreateFailed('helper-custom') + variantCreateFailed('helper-intel-only');

const studyDate = manifest.recorded_at?.slice(0, 10) || '2026-06-12';
const studyId = manifest.study_id;

let md = `# Firecracker snapshot portability on heterogeneous hosts

Empirical study: ${studyId} (${studyDate}) — ${manifest.distinct_cpu_count} distinct Intel CPU signatures on Azure VM-scale sets with nested KVM. Raw matrices: \`results/${studyId}/\`.

`;

const docsHeader = `${md}Raw data: [\`results/${studyId}/\`](../scripts/firecracker-snapshot-compat/results/${studyId}/)\n\n`;
const studyHeader = `${md}Raw data: \`results.jsonl\`, \`summary.md\` in this directory.\n\n`;

md += `## Summary

Firecracker snapshots freeze CPU state (timers, registers, CPUID) as well as memory. On a cloud scale set, hosts are not guaranteed to be CPU-identical — even under the same VM SKU — and Azure will keep rolling out newer hardware over time. You cannot assume that a snapshot taken on one runner will restore on another.

What works in practice: restore on the same CPU fingerprint as the creator, or on a small set of known-compatible hosts (often same generation, sometimes only in one direction). What does not work: treating the fleet as one interchangeable snapshot pool without checks.

CPU templates: In our study, the \`none\` variant means no \`cpu_template\` on \`/machine-config\` and no \`/cpu-config\` — the same as Firecracker’s default (\`cpu_template: None\`). Recommendation: do not use CPU templates for a heterogeneous Azure pool. Static templates (T2, C3, T2CL, T2S) did not make cross-host restore reliable and could produce invalid snapshots on some hosts. Custom intersected configs failed at create time. Stay on the host-native CPU model at snapshot time unless you run a homogeneous, validated fleet.

Running in production (snapshot on any host, restore on another):

1. Fingerprint + route — Record the host CPU fingerprint when creating a snapshot; only restore on hosts in a compatible allow-list (same fingerprint first; expand only where testing proves it).
2. Pools by generation — Split runners into snapshot pools per CPU generation; never move snapshots across pools. Re-baseline when Azure introduces new CPUs.
3. Graceful fallback — If restore fails compatibility checks or \`/snapshot/load\` errors, cold-start from the golden disk image instead of the snapshot (accept slower recovery rather than a broken VM).

## Why portability is limited

Snapshots are not like disk images. Restore asks the hypervisor to recreate low-level CPU state. If the target host is newer, older, or exposes different features (common on shared cloud hardware), restore fails even when the guest OS and workload are fine.

Two operational facts matter for any VMSS design:

- You may not be able to standardize on one CPU — the same SKU can map to different steppings; outliers appear over the fleet lifetime.
- The fleet will drift forward — new instance types and CPU generations join over time; snapshots from today are not automatically valid on tomorrow’s hosts.

## What breaks on restore

| Symptom | Underlying cause | Typical pattern |
|---------|------------------|-----------------|
| TSC / timer scaling | Guest time-stamp counter frequency in the snapshot cannot be adapted to the target host | Often older → newer generation fails; reverse may work |
| XCR / extended registers | Register state from the snapshot cannot be applied on the target | Newer → much older hosts (e.g. legacy cores in the same SKU family) |
| MSR mismatch | Model-specific registers differ between creator and restorer | Mixed generations or outliers |
| Invalid snapshot at create | Snapshot file already corrupt on the same host | Some CPU templates on drift hosts — restore never had a chance |

Same-host restore (or restore on a fingerprint-matched host) remained reliable in the study. Cross-host success was partial and directional, not universal.

## CPU templates and “cpu profiles”

### Is \`--cpu-profiles\` a Firecracker flag?

No. Firecracker has no \`--cpu-profiles\` CLI option. The colleague almost certainly meant one of:

- Static CPU templates — \`cpu_template\` on \`/machine-config\` (e.g. T2, C3, T2CL, T2S)
- Custom CPU config — JSON on \`/cpu-config\`, often built with \`cpu-template-helper\`

“CPU profiles” is informal wording for the same idea. It is not Node’s \`--cpu-prof\`, Azure Monitor profiling, or ACA sandbox tiers. If they meant something else (another runtime or orchestrator), confirm with them — nothing in our repo or Firecracker 1.14 matches that flag name.

### Is \`none\` the same as omitting templates?

Yes. Our \`none\` variant and production today both skip \`cpu_template\` and \`/cpu-config\`. Firecracker then uses the host’s native CPU model for the guest. That is the safest default for a mixed fleet.

### Template verdicts (study)

| Approach | Verdict | Notes |
|----------|---------|-------|
| No template (\`none\`) | Use | Baseline; same-host reliable; no extra create-time risk |
| Static templates (T2, C3, T2CL) | Avoid on mixed pools | No meaningful portability gain; can create invalid snapshots on some hosts |
| T2S | Avoid | Worst create-time failure rate in the study (${t2s.sameHostFail} same-host failures) |
| no-xcrs | No benefit (Intel-only) | Same cross-host outcomes as no template; does not fix TSC |
| Custom helper configs | Avoid | Intersected configs failed snapshot create on every host (${helperCreateFailed} cells) |

Templates can mask some CPUID differences in theory; they do not fix TSC across generations or outlier hardware in a scale set. Production restore uses \`/snapshot/load\` only — templates must be baked in at golden snapshot create, not applied at restore.

## Host environment (nested KVM on Azure)

On nested-KVM study VMs: virtualization features were available, but TSC scaling could not be tuned from the guest (\`kvm_intel.tsc_scaling\` not exposed). That reinforces treating cross-generation restore as best-effort, not guaranteed.

## Study specifics (reference)

The matrix used ${manifest.distinct_cpu_count} Intel signatures across D4s_v3 / D4s_v4 / D4s_v5 — including cases where the same SKU exposed different CPUs (e.g. modern Platinum vs legacy Broadwell on D4s_v3). Per-cell results: \`summary.md\`, \`results.jsonl\` under \`results/${studyId}/\`.
`;

fs.writeFileSync(outPath, studyHeader + md.slice(md.indexOf('## Summary')));
fs.writeFileSync(docsOutPath, docsHeader + md.slice(md.indexOf('## Summary')));
console.log(`Wrote ${outPath}`);
console.log(`Wrote ${docsOutPath}`);
