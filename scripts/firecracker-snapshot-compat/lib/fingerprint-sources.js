#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');

function readJSON(filePath) {
	return JSON.parse(fs.readFileSync(filePath, 'utf8'));
}

function isIntelFingerprint(doc) {
	const model = String(doc.cpu_model || '');
	if (/authenticamd/i.test(model)) {
		return false;
	}
	if (/intel/i.test(model)) {
		return true;
	}
	if (String(doc.kvm_intel_nested || '').trim()) {
		return true;
	}
	if (String(doc.kvm_amd_nested || '').trim()) {
		return false;
	}
	return true;
}

function resolveFingerprintPath(rootDir, fingerprintFile) {
	if (!fingerprintFile) {
		return null;
	}
	return path.isAbsolute(fingerprintFile) ? fingerprintFile : path.join(rootDir, fingerprintFile);
}

function walkFingerprintFiles(rootDir) {
	const fingerprintsRoot = path.join(rootDir, 'fingerprints');
	if (!fs.existsSync(fingerprintsRoot)) {
		return [];
	}
	const files = [];
	function walk(dir) {
		for (const entry of fs.readdirSync(dir)) {
			const full = path.join(dir, entry);
			const stat = fs.statSync(full);
			if (stat.isDirectory()) {
				walk(full);
				continue;
			}
			if (entry.endsWith('.json')) {
				files.push(full);
			}
		}
	}
	walk(fingerprintsRoot);
	return files.sort();
}

function representativeIntelFiles(rootDir, instancesPath) {
	const instancesFile = instancesPath || path.join(rootDir, 'instances.json');
	if (!fs.existsSync(instancesFile)) {
		return discoverFingerprintFiles(rootDir, { intelOnly: true });
	}
	const instancesDoc = readJSON(instancesFile);
	const seen = new Set();
	const files = [];
	for (const inst of [...(instancesDoc.instances || [])].sort((a, b) => a.index - b.index)) {
		const file = resolveFingerprintPath(rootDir, inst.fingerprint_file);
		if (!file || !fs.existsSync(file)) {
			continue;
		}
		let doc;
		try {
			doc = readJSON(file);
		} catch {
			continue;
		}
		if (!isIntelFingerprint(doc)) {
			continue;
		}
		const sig = inst.cpu_signature || doc.cpu_model || file;
		if (seen.has(sig)) {
			continue;
		}
		seen.add(sig);
		files.push(file);
	}
	return files;
}

function discoverFingerprintFiles(rootDir, { intelOnly = false, representatives = false, instancesPath = '' } = {}) {
	if (representatives) {
		const reps = representativeIntelFiles(rootDir, instancesPath);
		if (reps.length) {
			return reps;
		}
	}
	const files = walkFingerprintFiles(rootDir);
	if (!intelOnly) {
		return files;
	}
	return files.filter((file) => {
		try {
			return isIntelFingerprint(readJSON(file));
		} catch {
			return false;
		}
	});
}

if (require.main === module) {
	const [cmd, rootDir, ...rest] = process.argv.slice(2);
	if (cmd !== 'discover' || !rootDir) {
		console.error('usage: fingerprint-sources.js discover <compat-out-dir> [--intel-only] [--representatives] [--instances <path>]');
		process.exit(2);
	}
	let intelOnly = false;
	let representatives = false;
	let instancesPath = '';
	for (let i = 0; i < rest.length; i++) {
		if (rest[i] === '--intel-only') {
			intelOnly = true;
		} else if (rest[i] === '--representatives') {
			representatives = true;
		} else if (rest[i] === '--instances' && rest[i + 1]) {
			instancesPath = rest[++i];
		}
	}
	const files = discoverFingerprintFiles(rootDir, { intelOnly, representatives, instancesPath });
	process.stdout.write(`${files.join('\n')}\n`);
}

module.exports = {
	isIntelFingerprint,
	walkFingerprintFiles,
	discoverFingerprintFiles,
	representativeIntelFiles,
};
