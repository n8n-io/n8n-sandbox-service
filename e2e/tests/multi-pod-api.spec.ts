import { test, expect } from '@playwright/test';
import './matchers';
import { execWithTransientRetry, deleteSandbox, sandboxClient } from './helpers';

const BASE_URL_A = process.env.BASE_URL_A || 'http://localhost:18092';
const BASE_URL_B = process.env.BASE_URL_B || 'http://localhost:18093';

// Run from e2e/run-postgres-multi-pod.sh (two API pods, one Postgres, Docker runner).

test.describe('multi-pod API (Postgres)', () => {
  test('runner heartbeats on pod A; create and exec on pod B', async () => {
    const clientB = sandboxClient(BASE_URL_B);

    const record = await clientB.createSandbox();
    try {
      const result = await execWithTransientRetry(record.id, 'echo multi-pod', undefined, clientB);
      expect(result.stdout).toBe('multi-pod\n');
      expect(result).toHaveSucceeded();
    } finally {
      await deleteSandbox(record.id, clientB);
    }
  });

  test('sandbox created on pod B is visible on pod A', async () => {
    const clientA = sandboxClient(BASE_URL_A);
    const clientB = sandboxClient(BASE_URL_B);

    const record = await clientB.createSandbox();
    try {
      const got = await clientA.getSandbox(record.id);
      expect(got.id).toBe(record.id);
      expect(got.status).toBe('running');
    } finally {
      await deleteSandbox(record.id, clientB);
    }
  });
});
