import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import { nodePolyfills } from 'vite-plugin-node-polyfills';

const repoRoot = fileURLToPath(new URL('..', import.meta.url));
const sdkEntry = fileURLToPath(new URL('../sdk/src/index.ts', import.meta.url));
const axiosEntry = fileURLToPath(new URL('./node_modules/axios/index.js', import.meta.url));
const cryptoShim = fileURLToPath(new URL('./src/node-crypto.js', import.meta.url));

function polyfillShim(name) {
  return fileURLToPath(
    new URL(`./node_modules/vite-plugin-node-polyfills/shims/${name}/dist/index.js`, import.meta.url),
  );
}

export default defineConfig({
  root: 'src',
  plugins: [
    nodePolyfills({
      overrides: {
        crypto: cryptoShim,
      },
    }),
  ],
  resolve: {
    alias: {
      '@n8n/sandbox-client': sdkEntry,
      axios: axiosEntry,
      'vite-plugin-node-polyfills/shims/buffer': polyfillShim('buffer'),
      'vite-plugin-node-polyfills/shims/global': polyfillShim('global'),
      'vite-plugin-node-polyfills/shims/process': polyfillShim('process'),
    },
  },
  server: {
    fs: {
      allow: [repoRoot],
    },
  },
});
