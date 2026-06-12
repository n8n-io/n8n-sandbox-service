#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { summarizeFingerprints } = require('./fingerprint-summary');

function main() {
	const outDir = process.argv[2];
	const studyId = process.argv[3];
	const deploymentsFile = process.argv[4];
	const cohortFiles = process.argv.slice(5);
	if (!outDir || !studyId || cohortFiles.length === 0) {
		console.error('usage: merge-cohorts.js <out-dir> <study-id> <deployments-file> <cohort-instances.json>...');
		process.exit(1);
	}

	const cohorts = cohortFiles.map((file) => JSON.parse(fs.readFileSync(file, 'utf8')));
	const sshKeyPath = cohorts[0].ssh_key_path;
	const adminUsername = cohorts[0].admin_username;
	const resourceGroup = cohorts[0].resource_group;

	const merged = {
		study_id: studyId,
		resource_group: resourceGroup,
		ssh_key_path: sshKeyPath,
		admin_username: adminUsername,
		cohorts: [],
		instances: [],
	};

	let globalIndex = 0;
	const deployments = {
		study_id: studyId,
		resource_group: resourceGroup,
		cohorts: [],
	};

	for (const cohort of cohorts) {
		const cohortId = cohort.cohort_id;
		merged.cohorts.push({
			cohort_id: cohortId,
			deployment_name: cohort.deployment_name,
			location: cohort.location,
			vm_size: cohort.vm_size,
			cpu_analysis: cohort.cpu_analysis || null,
			instances: cohort.instances,
		});
		deployments.cohorts.push({
			cohort_id: cohortId,
			deployment_name: cohort.deployment_name,
			location: cohort.location,
			vm_size: cohort.vm_size,
		});
		for (const inst of cohort.instances) {
			merged.instances.push({
				...inst,
				index: globalIndex,
				cohort_id: cohortId,
				cohort_index: inst.index,
				fingerprint_file: `fingerprints/${cohortId}/instance-${inst.index}.json`,
			});
			globalIndex += 1;
		}
	}

	const fingerprintFiles = merged.instances.map((inst) => path.join(outDir, inst.fingerprint_file));
	const analysis = summarizeFingerprints(fingerprintFiles);
	merged.cpu_analysis = {
		homogeneous_across_all: analysis.homogeneous,
		distinct_cpu_count: analysis.distinct_cpu_count,
		cpu_signatures: analysis.cpu_signatures,
		groups: analysis.groups,
		interpretation: analysis.interpretation,
		cross_host_portability_meaningful: !analysis.homogeneous,
	};
	for (const inst of merged.instances) {
		const file = path.join(outDir, inst.fingerprint_file);
		const match = analysis.instances.find((e) => e.file === file);
		if (match) {
			inst.cpu_signature = match.cpu_signature;
			inst.cpu_model = match.cpu_model;
		}
	}
	for (const cohort of merged.cohorts) {
		const cohortInsts = merged.instances.filter((i) => i.cohort_id === cohort.cohort_id);
		const sigs = [...new Set(cohortInsts.map((i) => i.cpu_signature))];
		cohort.cpu_homogeneous = sigs.length <= 1;
		cohort.distinct_cpu_count = sigs.length;
	}

	fs.writeFileSync(path.join(outDir, 'instances.json'), `${JSON.stringify(merged, null, 2)}\n`);
	fs.writeFileSync(deploymentsFile, `${JSON.stringify(deployments, null, 2)}\n`);
}

if (require.main === module) {
	main();
}
