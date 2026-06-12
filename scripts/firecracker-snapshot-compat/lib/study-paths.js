#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');

function readJSON(filePath) {
	return JSON.parse(fs.readFileSync(filePath, 'utf8'));
}

function runIdFromDoc(doc) {
	return doc.study_id || doc.deployment_name || doc.cohort_id || 'compat-run';
}

function runIdFromManifest(manifest) {
	return manifest.run_id || manifest.study_id || manifest.deployment_name || manifest.cohort_id || 'compat-run';
}

function studyDir(outDir, runId) {
	return path.join(outDir, 'results', runId);
}

function studyPaths(outDir, runId) {
	const dir = studyDir(outDir, runId);
	return {
		studyDir: dir,
		resultsFile: path.join(dir, 'results.jsonl'),
		manifestFile: path.join(dir, 'run-manifest.json'),
		planFile: path.join(dir, 'matrix-plan.json'),
		planTsv: path.join(dir, 'matrix-plan.tsv'),
		summaryFile: path.join(dir, 'summary.md'),
		instancesSnapshot: path.join(dir, 'instances.json'),
		deploymentsSnapshot: path.join(dir, 'deployments.json'),
		fingerprintsDir: path.join(dir, 'fingerprints'),
	};
}

function resolveStudyDir(outDir, { runId, instancesPath } = {}) {
	if (runId) {
		return studyDir(outDir, runId);
	}
	const latestLink = path.join(outDir, 'results', 'latest');
	if (fs.existsSync(latestLink)) {
		const target = fs.readlinkSync(latestLink);
		return path.isAbsolute(target) ? target : path.join(outDir, 'results', target);
	}
	if (instancesPath && fs.existsSync(instancesPath)) {
		return studyDir(outDir, runIdFromDoc(readJSON(instancesPath)));
	}
	return null;
}

function updateLatestLink(outDir, runId) {
	const resultsRoot = path.join(outDir, 'results');
	fs.mkdirSync(resultsRoot, { recursive: true });
	const latest = path.join(resultsRoot, 'latest');
	try {
		fs.unlinkSync(latest);
	} catch (err) {
		if (err.code !== 'ENOENT') {
			throw err;
		}
	}
	fs.symlinkSync(runId, latest);
}

if (require.main === module) {
	const [cmd, ...args] = process.argv.slice(2);
	if (cmd === 'run-id' && args[0]) {
		process.stdout.write(runIdFromDoc(readJSON(args[0])));
	} else if (cmd === 'study-dir' && args[0] && args[1]) {
		process.stdout.write(studyDir(args[0], args[1]));
	} else if (cmd === 'resolve' && args[0]) {
		const dir = resolveStudyDir(args[0], { instancesPath: args[1] });
		if (!dir) {
			process.exit(1);
		}
		process.stdout.write(dir);
	} else {
		console.error('usage: study-paths.js run-id <instances.json>');
		console.error('       study-paths.js study-dir <out-dir> <run-id>');
		console.error('       study-paths.js resolve <out-dir> [instances.json]');
		process.exit(2);
	}
}

module.exports = {
	runIdFromDoc,
	runIdFromManifest,
	studyDir,
	studyPaths,
	resolveStudyDir,
	updateLatestLink,
};
