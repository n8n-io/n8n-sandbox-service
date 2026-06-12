#!/usr/bin/env node
'use strict';

const fs = require('fs');
const { instanceContext, readInstances } = require('./instance-context');

function appendMatrixResult({
	resultsPath,
	instancesPath,
	runId,
	matrixMode,
	variant,
	creator,
	restorer,
	status,
	message,
	portabilityTest,
}) {
	const doc = readInstances(instancesPath);
	const creatorCtx = instanceContext(doc, creator);
	const restorerCtx = instanceContext(doc, restorer);
	let crossHostMeaningful = portabilityTest;
	if (crossHostMeaningful === undefined || crossHostMeaningful === '') {
		if (creator === restorer) {
			crossHostMeaningful = true;
		} else if (
			creatorCtx.cpu_signature &&
			restorerCtx.cpu_signature &&
			creatorCtx.cpu_signature === restorerCtx.cpu_signature
		) {
			crossHostMeaningful = false;
		} else {
			crossHostMeaningful = true;
		}
	}

	const row = {
		run_id: runId,
		matrix_mode: matrixMode,
		variant,
		creator_index: creator,
		restorer_index: restorer,
		cohort_id: creatorCtx.cohort_id,
		creator_cpu_signature: creatorCtx.cpu_signature,
		restorer_cpu_signature: restorerCtx.cpu_signature,
		creator_cpu_model: creatorCtx.cpu_model,
		restorer_cpu_model: restorerCtx.cpu_model,
		creator_vm_size: creatorCtx.vm_size,
		restorer_vm_size: restorerCtx.vm_size,
		creator_location: creatorCtx.location,
		restorer_location: restorerCtx.location,
		creator_cohort_id: creatorCtx.cohort_id,
		restorer_cohort_id: restorerCtx.cohort_id,
		cross_host_portability_test: crossHostMeaningful,
		status,
		message,
		recorded_at: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
	};
	fs.appendFileSync(resultsPath, `${JSON.stringify(row)}\n`);
}

if (require.main === module) {
	const [
		,
		,
		resultsPath,
		instancesPath,
		runId,
		matrixMode,
		variant,
		creator,
		restorer,
		status,
		portabilityTest,
		messagePath,
	] = process.argv;
	const message = fs.readFileSync(messagePath, 'utf8');
	appendMatrixResult({
		resultsPath,
		instancesPath,
		runId,
		matrixMode,
		variant,
		creator: Number(creator),
		restorer: Number(restorer),
		status,
		message,
		portabilityTest: portabilityTest === '' ? undefined : portabilityTest,
	});
}

module.exports = { appendMatrixResult };
