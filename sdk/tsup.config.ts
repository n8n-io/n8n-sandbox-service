import { defineConfig } from 'tsup';

export default defineConfig({
	entry: ['src/index.ts'],
	format: ['cjs', 'esm'],
	dts: {
		compilerOptions: {
			incremental: false,
		},
	},
	clean: true,
	minify: true,
	sourcemap: true,
});
