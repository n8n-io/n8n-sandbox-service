#!/usr/bin/env node
'use strict';

function classifyFailure(phase, output, creator, restorer) {
	const text = String(output ?? '');
	const lines = text.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
	const sameHost =
		creator !== undefined &&
		creator !== '' &&
		restorer !== undefined &&
		restorer !== '' &&
		String(creator) === String(restorer);

	const systemicPatterns = [
		'Loading a microVM snapshot not allowed after configuring boot-specific resources',
		'ERROR: timed out waiting for Firecracker API socket',
		'ERROR: snapshot files not found',
		'ERROR: snapshot files missing or empty',
		'scp: illegal option',
	];
	for (const p of systemicPatterns) {
		if (text.includes(p)) {
			return { kind: 'systemic', summary: p };
		}
	}

	const snapshotRes = [
		/bincode decoding/i,
		/UnexpectedEnd/i,
		/Failed to get snapshot state from file/i,
		/Failed to load snapshot state from file/i,
	];
	for (const re of snapshotRes) {
		const line = lines.find((l) => re.test(l));
		if (line) {
			return {
				kind: sameHost ? 'snapshot-same-host' : 'snapshot',
				summary: line,
			};
		}
	}

	const portabilityRes = [
		/sandbox daemon did not become healthy/i,
		/vcpu xcrs/i,
		/TSC scaling/i,
		/vendor ID differs/i,
		/Failed to set all KVM MSRs/i,
		/Failed to restore vCPUs/i,
		/cpuid/i,
		/incompat/i,
		/unsupported.*feature/i,
		/host feature/i,
		/migrat/i,
	];
	for (const re of portabilityRes) {
		const line = lines.find((l) => re.test(l));
		if (line) {
			return { kind: 'portability', summary: line };
		}
	}

	const errLine =
		[...lines].reverse().find((l) => /error/i.test(l)) ||
		lines.slice(-3).join(' | ') ||
		'unknown error';
	return { kind: 'other', summary: errLine };
}

function failureKindFromMessage(message, creator, restorer) {
	return classifyFailure('restore', message, creator, restorer).kind;
}

if (require.main === module) {
	const [, , phase = '', output = '', creator = '', restorer = ''] = process.argv;
	const { kind, summary } = classifyFailure(phase, output, creator, restorer);
	process.stdout.write(`${kind}\t${summary}`);
}

module.exports = { classifyFailure, failureKindFromMessage };
