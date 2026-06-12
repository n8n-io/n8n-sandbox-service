#!/usr/bin/env node
'use strict';

const fs = require('fs');

function load(path) {
	return JSON.parse(fs.readFileSync(path, 'utf8'));
}

function keyModifier(m) {
	return `${m.leaf}:${m.subleaf}:${m.flags}`;
}

function keyReg(mod, reg) {
	return `${keyModifier(mod)}:${reg.register}`;
}

function andBitmaps(a, b) {
	const parse = (s) => BigInt(s.startsWith('0b') ? s.slice(2) : s);
	const width = Math.max(a.length, b.length) - (a.startsWith('0b') ? 2 : 0);
	const av = parse(a);
	const bv = parse(b);
	const mask = (1n << BigInt(width)) - 1n;
	return '0b' + (av & bv & mask).toString(2).padStart(width, '0');
}

function intersectConfigs(configs) {
	if (configs.length === 0) {
		return { kvm_capabilities: [], cpuid_modifiers: [], msr_modifiers: [] };
	}
	if (configs.length === 1) {
		return configs[0];
	}

	const kvm = configs
		.map((c) => new Set(c.kvm_capabilities || []))
		.reduce((acc, set) => new Set([...acc].filter((x) => set.has(x))));

	const maps = configs.map((c) => {
		const m = new Map();
		for (const mod of c.cpuid_modifiers || []) {
			for (const reg of mod.modifiers || []) {
				m.set(keyReg(mod, reg), { mod, reg });
			}
		}
		return m;
	});

	const commonKeys = [...maps[0].keys()].filter((k) => maps.every((m) => m.has(k)));
	const cpuid_modifiers = [];
	const byModifier = new Map();

	for (const k of commonKeys) {
		let { mod, reg } = maps[0].get(k);
		let bitmap = reg.bitmap;
		for (let i = 1; i < maps.length; i++) {
			bitmap = andBitmaps(bitmap, maps[i].get(k).reg.bitmap);
		}
		const mk = keyModifier(mod);
		if (!byModifier.has(mk)) {
			byModifier.set(mk, { leaf: mod.leaf, subleaf: mod.subleaf, flags: mod.flags, modifiers: [] });
		}
		byModifier.get(mk).modifiers.push({ register: reg.register, bitmap });
	}
	for (const v of byModifier.values()) {
		cpuid_modifiers.push(v);
	}

	return {
		kvm_capabilities: [...kvm],
		cpuid_modifiers,
		msr_modifiers: [],
	};
}

if (require.main === module) {
	const [outPath, ...inputs] = process.argv.slice(2);
	if (!outPath || inputs.length === 0) {
		console.error('usage: intersect-cpu-configs.js <out.json> <dump1.json> [...]');
		process.exit(2);
	}
	const result = intersectConfigs(inputs.map(load));
	fs.writeFileSync(outPath, JSON.stringify(result, null, 2) + '\n');
	console.error(`==> Intersected ${inputs.length} dumps -> ${outPath}`);
}

module.exports = { intersectConfigs };
