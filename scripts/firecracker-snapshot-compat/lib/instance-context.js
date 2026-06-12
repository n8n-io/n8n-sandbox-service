#!/usr/bin/env node
'use strict';

const fs = require('fs');

function readInstances(instancesPath) {
	return JSON.parse(fs.readFileSync(instancesPath, 'utf8'));
}

function cohortMeta(doc, cohortId) {
	if (doc.cohorts) {
		const cohort = doc.cohorts.find((c) => c.cohort_id === cohortId);
		if (cohort) {
			return { vm_size: cohort.vm_size || '', location: cohort.location || '' };
		}
	}
	return { vm_size: doc.vm_size || '', location: doc.location || '' };
}

function instanceContext(doc, index) {
	const inst =
		doc.instances.find((i) => i.index === index) ||
		doc.instances[index] ||
		{};
	const cohortId = inst.cohort_id || doc.cohort_id || '';
	const { vm_size, location } = cohortMeta(doc, cohortId);
	return {
		index: inst.index ?? index,
		name: inst.name || '',
		cohort_id: cohortId,
		cpu_signature: inst.cpu_signature || '',
		cpu_model: inst.cpu_model || '',
		vm_size,
		location,
	};
}

if (require.main === module) {
	const instancesPath = process.argv[2];
	const index = Number(process.argv[3]);
	process.stdout.write(JSON.stringify(instanceContext(readInstances(instancesPath), index)));
}

module.exports = { instanceContext, cohortMeta, readInstances };
