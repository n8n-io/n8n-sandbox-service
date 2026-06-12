#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { runIdFromManifest, studyDir, updateLatestLink } = require('./study-paths');

function copyFileIfMissing(src, dest) {
	if (!fs.existsSync(src)) {
		return false;
	}
	if (fs.existsSync(dest)) {
		return false;
	}
	fs.mkdirSync(path.dirname(dest), { recursive: true });
	fs.copyFileSync(src, dest);
	return true;
}

function moveFileIfExists(src, dest) {
	if (!fs.existsSync(src)) {
		return false;
	}
	fs.mkdirSync(path.dirname(dest), { recursive: true });
	if (fs.existsSync(dest)) {
		fs.unlinkSync(src);
		return false;
	}
	fs.renameSync(src, dest);
	return true;
}

function mergeDir(srcDir, destDir) {
	if (!fs.existsSync(srcDir)) {
		return;
	}
	fs.mkdirSync(destDir, { recursive: true });
	for (const entry of fs.readdirSync(srcDir)) {
		const src = path.join(srcDir, entry);
		const dest = path.join(destDir, entry);
		const stat = fs.statSync(src);
		if (stat.isDirectory()) {
			mergeDir(src, dest);
			continue;
		}
		copyFileIfMissing(src, dest);
	}
}

function appendJsonl(dest, lines) {
	if (!lines.length) {
		return;
	}
	fs.mkdirSync(path.dirname(dest), { recursive: true });
	const prefix = fs.existsSync(dest) && fs.statSync(dest).size > 0 ? '\n' : '';
	fs.appendFileSync(dest, `${prefix}${lines.join('\n')}\n`);
}

function migrateLegacyJsonl(resultsRoot) {
	const legacyJsonl = path.join(resultsRoot, 'results.jsonl');
	if (!fs.existsSync(legacyJsonl)) {
		return [];
	}
	const lines = fs.readFileSync(legacyJsonl, 'utf8').trim().split('\n').filter(Boolean);
	const byRun = new Map();
	for (const line of lines) {
		const row = JSON.parse(line);
		const runId = row.run_id || 'unknown';
		if (!byRun.has(runId)) {
			byRun.set(runId, []);
		}
		byRun.get(runId).push(line);
	}
	const migrated = [];
	for (const [runId, rows] of byRun.entries()) {
		const dest = path.join(studyDir(path.dirname(resultsRoot), runId), 'results.jsonl');
		appendJsonl(dest, rows);
		migrated.push({ runId, rows: rows.length, dest });
	}
	fs.unlinkSync(legacyJsonl);
	return migrated;
}

function migrateStudiesDir(resultsRoot) {
	const studiesDir = path.join(resultsRoot, 'studies');
	if (!fs.existsSync(studiesDir)) {
		return [];
	}
	const migrated = [];
	for (const entry of fs.readdirSync(studiesDir)) {
		const src = path.join(studiesDir, entry);
		if (!fs.statSync(src).isDirectory()) {
			continue;
		}
		const dest = path.join(resultsRoot, entry);
		mergeDir(src, dest);
		migrated.push(entry);
	}
	fs.rmSync(studiesDir, { recursive: true, force: true });
	return migrated;
}

function migrateTopLevelManifests(resultsRoot) {
	const outDir = path.dirname(resultsRoot);
	const migrated = [];
	for (const entry of fs.readdirSync(resultsRoot)) {
		if (!entry.startsWith('run-manifest') || !entry.endsWith('.json')) {
			continue;
		}
		const src = path.join(resultsRoot, entry);
		const manifest = JSON.parse(fs.readFileSync(src, 'utf8'));
		const runId = runIdFromManifest(manifest);
		const destDir = studyDir(outDir, runId);
		fs.mkdirSync(destDir, { recursive: true });
		const destManifest = path.join(destDir, 'run-manifest.json');
		copyFileIfMissing(src, destManifest);
		if (entry !== 'run-manifest.json') {
			copyFileIfMissing(src, path.join(destDir, entry));
		}
		fs.unlinkSync(src);
		migrated.push({ runId, file: entry });
	}
	return migrated;
}

function migrateLooseArtifacts(resultsRoot, activeRunId) {
	if (!activeRunId) {
		return [];
	}
	const destDir = studyDir(path.dirname(resultsRoot), activeRunId);
	fs.mkdirSync(destDir, { recursive: true });
	const moved = [];
	for (const name of ['matrix-plan.json', 'matrix-plan.tsv', 'summary.md']) {
		if (moveFileIfExists(path.join(resultsRoot, name), path.join(destDir, name))) {
			moved.push(name);
		}
	}
	return moved;
}

function migrate(outDir) {
	const resultsRoot = path.join(outDir, 'results');
	if (!fs.existsSync(resultsRoot)) {
		fs.mkdirSync(resultsRoot, { recursive: true });
		return { migrated: false, reason: 'created empty results root' };
	}

	const jsonlMigrated = migrateLegacyJsonl(resultsRoot);
	const studiesMigrated = migrateStudiesDir(resultsRoot);
	const manifestsMigrated = migrateTopLevelManifests(resultsRoot);

	let activeRunId = null;
	const instancesPath = path.join(outDir, 'instances.json');
	if (fs.existsSync(instancesPath)) {
		const doc = JSON.parse(fs.readFileSync(instancesPath, 'utf8'));
		activeRunId = doc.study_id || doc.deployment_name || doc.cohort_id || null;
	}
	if (!activeRunId && jsonlMigrated.length === 1) {
		activeRunId = jsonlMigrated[0].runId;
	}

	const looseMoved = migrateLooseArtifacts(resultsRoot, activeRunId);
	if (activeRunId) {
		updateLatestLink(outDir, activeRunId);
	}

	return {
		migrated: true,
		jsonlMigrated,
		studiesMigrated,
		manifestsMigrated,
		looseMoved,
		activeRunId,
		latest: activeRunId,
	};
}

if (require.main === module) {
	const outDir = process.argv[2];
	if (!outDir) {
		console.error('usage: migrate-results-layout.js <compat-out-dir>');
		process.exit(2);
	}
	const report = migrate(outDir);
	console.log(JSON.stringify(report, null, 2));
}

module.exports = { migrate };
