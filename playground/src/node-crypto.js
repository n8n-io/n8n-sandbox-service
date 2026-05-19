/**
 * Browser-only replacement for the SDK's `node:crypto` import.
 *
 * Vite aliases `crypto` to this module so the playground can run the SDK
 * without bundling Node's crypto polyfill. The SDK only needs `randomUUID`,
 * which is implemented with Web Crypto.
 */
function randomBytes(length) {
  if (!globalThis.crypto?.getRandomValues) {
    throw new Error('crypto.getRandomValues is not available');
  }
  return globalThis.crypto.getRandomValues(new Uint8Array(length));
}

export function randomUUID() {
  if (globalThis.crypto?.randomUUID) {
    return globalThis.crypto.randomUUID();
  }

  const bytes = randomBytes(16);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;

  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0'));
  return [
    hex.slice(0, 4).join(''),
    hex.slice(4, 6).join(''),
    hex.slice(6, 8).join(''),
    hex.slice(8, 10).join(''),
    hex.slice(10, 16).join(''),
  ].join('-');
}

export default { randomUUID };
