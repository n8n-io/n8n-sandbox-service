#!/usr/bin/env node
'use strict';

const fs = require('fs');

function loadRows(resultsPath) {
	if (!fs.existsSync(resultsPath)) {
		return [];
	}
	return fs
		.readFileSync(resultsPath, 'utf8')
		.trim()
		.split('\n')
		.filter(Boolean)
		.map((line) => JSON.parse(line));
}

function resultKey(row) {
	return `${row.run_id}\t${row.matrix_mode || ''}\t${row.variant}\t${row.creator_index}\t${row.restorer_index}`;
}

function hasResult(rows, { runId, mode, variant, creator, restorer }) {
	return rows.some(
		(row) =>
			row.run_id === runId &&
			(row.matrix_mode || '') === mode &&
			row.variant === variant &&
			row.creator_index === creator &&
			row.restorer_index === restorer,
	);
}

function creatorCreateSucceeded(rows, { runId, mode, variant, creator }) {
	return rows.some(
		(row) =>
			row.run_id === runId &&
			(row.matrix_mode || '') === mode &&
			row.variant === variant &&
			row.creator_index === creator &&
			row.status !== 'create_failed',
	);
}

/** Keep the newest row per matrix cell (for summaries across append/resume). */
function latestRows(rows, { runId } = {}) {
	const filtered = runId ? rows.filter((row) => row.run_id === runId) : rows;
	const byKey = new Map();
	for (const row of filtered) {
		const key = resultKey(row);
		const prev = byKey.get(key);
		if (!prev || String(row.recorded_at || '') >= String(prev.recorded_at || '')) {
			byKey.set(key, row);
		}
	}
	return [...byKey.values()];
}

if (require.main === module) {
	const [cmd, resultsPath, ...args] = process.argv.slice(2);
	const rows = loadRows(resultsPath);
	if (cmd === 'has') {
		const [runId, mode, variant, creator, restorer] = args;
		process.exit(
			hasResult(rows, {
				runId,
				mode,
				variant,
				creator: Number(creator),
				restorer: Number(restorer),
			})
				? 0
				: 1,
		);
	}
	if (cmd === 'create-ok') {
		const [runId, mode, variant, creator] = args;
		process.exit(
			creatorCreateSucceeded(rows, {
				runId,
				mode,
				variant,
				creator: Number(creator),
			})
				? 0
				: 1,
		);
	}
	console.error('usage: matrix-results.js has <file> <runId> <mode> <variant> <creator> <restorer>');
	console.error('       matrix-results.js create-ok <file> <runId> <mode> <variant> <creator>');
	process.exit(2);
}

module.exports = { loadRows, hasResult, creatorCreateSucceeded, latestRows, resultKey };
