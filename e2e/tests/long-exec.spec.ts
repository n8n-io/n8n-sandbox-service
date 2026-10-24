import { test, expect } from '@playwright/test';
import './matchers';
import { client, createSandbox, deleteSandbox, ensureTenantAuth } from './helpers';

// A multi-hour execution completes on the factory pool's config: the runner's
// exec cap raised (E2E_RUNNER_MAX_EXEC_TIMEOUT=8h), the stream held through
// the API for the whole run, and the sandbox kept running past the global 1 h
// idle-stop window. Opt-in: E2E_LONG_EXEC_HOURS sets the run's length.
test.describe('Long execution', () => {
  const hours = Number(process.env.E2E_LONG_EXEC_HOURS || 0);

  test('a multi-hour execution completes', async () => {
    test.skip(!hours, 'set E2E_LONG_EXEC_HOURS (and E2E_RUNNER_MAX_EXEC_TIMEOUT) to run');
    const seconds = Math.round(hours * 3600);
    test.setTimeout((seconds + 1800) * 1000);
    await ensureTenantAuth();
    const id = await createSandbox();
    try {
      const started = Date.now();
      const result = await client.exec(id, {
        command: `sleep ${seconds} && echo long-run-done`,
        timeoutMs: (seconds + 900) * 1000,
      });
      expect(result).toHaveSucceeded();
      expect(result.stdout.trim()).toBe('long-run-done');
      expect(Date.now() - started).toBeGreaterThanOrEqual(seconds * 1000);
      expect((await client.getSandbox(id)).status).toBe('running');
    } finally {
      await deleteSandbox(id);
    }
  });
});
