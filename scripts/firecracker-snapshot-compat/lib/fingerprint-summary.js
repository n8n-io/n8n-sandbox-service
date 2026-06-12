#!/usr/bin/env node
'use strict';

const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const { studyPaths, updateLatestLink } = require('./study-paths');

function readJSON(filePath) {
	return JSON.parse(fs.readFileSync(filePath, 'utf8'));
}

function cpuSignature(fp) {
	const material = [
		fp.cpu_model || '',
		fp.cpu_flags || '',
		fp.kvm_intel_nested || '',
		fp.kvm_intel_ept || '',
		fp.kvm_intel_unrestricted_guest || '',
		fp.kvm_intel_tsc_scaling || '',
		fp.kvm_amd_nested || '',
	].join('\n');
	return crypto.createHash('sha256').update(material).digest('hex').slice(0, 16);
}

function summarizeFingerprints(fingerprintFiles) {
	const entries = fingerprintFiles.map((file) => ({
		file,
		data: readJSON(file),
	}));
	const enriched = entries.map(({ file, data }) => ({
		file,
		azure_name: data.azure_name || data.hostname || path.basename(file, '.json'),
		azure_vm_size: data.azure_vm_size || '',
		cpu_model: data.cpu_model || '',
		cpu_signature: cpuSignature(data),
	}));
	const signatures = [...new Set(enriched.map((e) => e.cpu_signature))];
	const homogeneous = signatures.length <= 1;
	const groups = signatures.map((signature) => ({
		cpu_signature: signature,
		cpu_model: enriched.find((e) => e.cpu_signature === signature)?.cpu_model || '',
		instances: enriched.filter((e) => e.cpu_signature === signature).map((e) => e.azure_name),
	}));
	return {
		homogeneous,
		distinct_cpu_count: signatures.length,
		cpu_signatures: signatures,
		groups,
		instances: enriched,
		interpretation: homogeneous
			? 'All instances share identical guest-visible CPU features. Cross-instance restore results do not demonstrate portability across heterogeneous hosts.'
			: `${signatures.length} distinct CPU signatures detected. Cross-signature restore cells test host portability.`,
	};
}

function loadFingerprintFiles(outDir, instances) {
	const files = [];
	for (const inst of instances) {
		const rel = inst.fingerprint_file || '';
		const file = path.isAbsolute(rel) ? rel : path.join(outDir, rel);
		if (!fs.existsSync(file)) {
			throw new Error(`fingerprint not found: ${file}`);
		}
		files.push(file);
	}
	return files;
}

function main() {
	const mode = process.argv[2];
	const outDir = process.argv[3];
	if (!mode || !outDir) {
		console.error('usage: fingerprint-summary.js analyze <out-dir>');
		process.exit(1);
	}

	const instancesPath = path.join(outDir, 'instances.json');
	const instancesDoc = readJSON(instancesPath);
	const flatInstances = instancesDoc.instances || [];
	const fingerprintFiles = loadFingerprintFiles(outDir, flatInstances);
	const analysis = summarizeFingerprints(fingerprintFiles);

	for (const inst of flatInstances) {
		const rel = inst.fingerprint_file || '';
		const file = path.isAbsolute(rel) ? rel : path.join(outDir, rel);
		const match = analysis.instances.find((e) => e.file === file);
		if (match) {
			inst.cpu_signature = match.cpu_signature;
			inst.cpu_model = match.cpu_model;
		}
	}

	instancesDoc.cpu_analysis = analysis;
	fs.writeFileSync(instancesPath, `${JSON.stringify(instancesDoc, null, 2)}\n`);

	const runId =
		instancesDoc.study_id ||
		instancesDoc.deployment_name ||
		instancesDoc.cohort_id ||
		'compat-run';
	const manifest = {
		run_id: runId,
		recorded_at: new Date().toISOString(),
		deployment_name: instancesDoc.deployment_name || null,
		study_id: instancesDoc.study_id || null,
		cohort_id: instancesDoc.cohort_id || null,
		location: instancesDoc.location || null,
		vm_size: instancesDoc.vm_size || null,
		resource_group: instancesDoc.resource_group || null,
		instance_count: flatInstances.length,
		cpu_homogeneous: analysis.homogeneous,
		distinct_cpu_count: analysis.distinct_cpu_count,
		cpu_signatures: analysis.cpu_signatures,
		cpu_groups: analysis.groups,
		interpretation: analysis.interpretation,
		cross_host_portability_meaningful: !analysis.homogeneous,
		instances: flatInstances.map((inst) => ({
			index: inst.index,
			name: inst.name,
			ip: inst.ip,
			cpu_signature: inst.cpu_signature,
			cpu_model: inst.cpu_model,
			cohort_id: inst.cohort_id || instancesDoc.cohort_id || null,
		})),
	};
	const paths = studyPaths(outDir, runId);
	fs.mkdirSync(paths.studyDir, { recursive: true });
	const manifestJson = `${JSON.stringify(manifest, null, 2)}\n`;
	fs.writeFileSync(paths.manifestFile, manifestJson);
	writeStudySnapshot(outDir, paths, instancesDoc, flatInstances, manifest);
	updateLatestLink(outDir, runId);
	process.stdout.write(JSON.stringify(analysis, null, 2));
}

function copyFileIfExists(src, dest) {
	if (!fs.existsSync(src)) {
		return;
	}
	fs.mkdirSync(path.dirname(dest), { recursive: true });
	fs.copyFileSync(src, dest);
}

function writeStudySnapshot(outDir, paths, instancesDoc, flatInstances, manifest) {
	copyFileIfExists(path.join(outDir, 'instances.json'), paths.instancesSnapshot);
	copyFileIfExists(path.join(outDir, 'deployments.json'), paths.deploymentsSnapshot);

	for (const inst of flatInstances) {
		const rel = inst.fingerprint_file;
		if (!rel) {
			continue;
		}
		const src = path.isAbsolute(rel) ? rel : path.join(outDir, rel);
		const dest = path.join(paths.fingerprintsDir, rel.replace(/^fingerprints\//, ''));
		copyFileIfExists(src, dest);
	}

	const archiveMeta = {
		study_id: manifest.run_id,
		archived_at: new Date().toISOString(),
		deployment_name: instancesDoc.deployment_name || null,
		cohort_id: instancesDoc.cohort_id || null,
		instance_count: flatInstances.length,
		cpu_homogeneous: manifest.cpu_homogeneous,
		distinct_cpu_count: manifest.distinct_cpu_count,
	};
	fs.writeFileSync(path.join(paths.studyDir, 'archive-meta.json'), `${JSON.stringify(archiveMeta, null, 2)}\n`);
}

if (require.main === module) {
	main();
}

module.exports = { cpuSignature, summarizeFingerprints };
