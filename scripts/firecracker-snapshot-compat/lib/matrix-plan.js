#!/usr/bin/env node
'use strict';

const fs = require('fs');

function readInstances(path) {
	return JSON.parse(fs.readFileSync(path, 'utf8'));
}

function representativeBySignature(instances) {
	const reps = new Map();
	for (const inst of [...instances].sort((a, b) => a.index - b.index)) {
		const sig = inst.cpu_signature || `unknown-${inst.index}`;
		if (!reps.has(sig)) {
			reps.set(sig, inst.index);
		}
	}
	return reps;
}

function buildPlan(instancesDoc, mode) {
	const instances = instancesDoc.instances || [];
	const variants = (process.env.COMPAT_MATRIX_VARIANTS || 'none,T2,C3,T2S,T2CL,helper-custom,helper-intel-only,no-xcrs').split(',');
	const createAll = process.env.COMPAT_MATRIX_CREATE_ALL === '1';
	const reps = representativeBySignature(instances);
	const signatures = [...reps.keys()];

	const creates = [];
	for (const variant of variants) {
		if (createAll || mode === 'full') {
			for (const inst of instances) {
				creates.push({
					variant,
					creator: inst.index,
					cpu_signature: inst.cpu_signature || null,
					cohort_id: inst.cohort_id || null,
				});
			}
			continue;
		}
		for (const [sig, creator] of reps.entries()) {
			const inst = instances.find((i) => i.index === creator);
			creates.push({
				variant,
				creator,
				cpu_signature: sig,
				cohort_id: inst?.cohort_id || null,
			});
		}
	}

	const restores = [];
	for (const create of creates) {
		if (mode === 'full') {
			for (const inst of instances) {
				restores.push({
					variant: create.variant,
					creator: create.creator,
					restorer: inst.index,
					same_host: create.creator === inst.index,
					portability_test: create.creator !== inst.index,
					skipped: false,
				});
			}
			continue;
		}

		restores.push({
			variant: create.variant,
			creator: create.creator,
			restorer: create.creator,
			same_host: true,
			portability_test: false,
			skipped: false,
		});

		const creatorSig = create.cpu_signature || `unknown-${create.creator}`;
		for (const [restorerSig, restorer] of reps.entries()) {
			if (restorerSig === creatorSig) {
				continue;
			}
			restores.push({
				variant: create.variant,
				creator: create.creator,
				restorer,
				same_host: false,
				portability_test: true,
				skipped: false,
				creator_cpu_signature: creatorSig,
				restorer_cpu_signature: restorerSig,
			});
		}
	}

	const fullRestoreCount = creates.length * instances.length;
	const fullCreateCount = variants.length * instances.length;
	return {
		mode,
		variant_count: variants.length,
		instance_count: instances.length,
		distinct_cpu_signatures: signatures.length,
		create_count: creates.length,
		restore_count: restores.length,
		full_matrix_create_count: fullCreateCount,
		full_matrix_restore_count: fullRestoreCount,
		restores,
		creates,
	};
}

function printTsv(plan) {
	for (const create of plan.creates) {
		process.stdout.write(`create\t${create.variant}\t${create.creator}\n`);
	}
	for (const restore of plan.restores) {
		process.stdout.write(
			`restore\t${restore.variant}\t${restore.creator}\t${restore.restorer}\t${restore.same_host}\t${restore.portability_test}\n`,
		);
	}
}

function main() {
	const instancesPath = process.argv[2];
	const mode = process.argv[3] || process.env.COMPAT_MATRIX_MODE || 'smart';
	const format = process.argv[4] || 'json';
	if (!instancesPath) {
		console.error('usage: matrix-plan.js <instances.json> [full|smart] [json|tsv]');
		process.exit(1);
	}
	const plan = buildPlan(readInstances(instancesPath), mode);
	if (format === 'tsv') {
		printTsv(plan);
		return;
	}
	process.stdout.write(`${JSON.stringify(plan, null, 2)}\n`);
}

if (require.main === module) {
	main();
}

module.exports = { buildPlan, representativeBySignature };
