import { test, expect } from '@playwright/test';
import {
  ADMIN_API_KEY,
  BASE_URL,
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  runnerCordon,
  runnerCordonConfigured,
  scrapeRunnerMetrics,
  stopSandboxViaRunner,
} from './helpers';
import { placements, placementsSince, postCreate } from './capacity-helpers';
import { parseGauge } from './metrics-helpers';

/** Waits until GET /admin/runners reports every runner with the given cordon state. */
async function waitForAdminCordoned(want: boolean, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const res = await fetch(`${BASE_URL}/admin/runners`, { headers: { 'X-Api-Key': ADMIN_API_KEY } });
    const body = (await res.json()) as { runners: { cordoned: boolean }[] };
    if (body.runners.length > 0 && body.runners.every((r) => r.cordoned === want)) return;
    if (Date.now() > deadline) {
      throw new Error(`/admin/runners did not report cordoned=${want} within ${timeoutMs}ms: ${JSON.stringify(body)}`);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
}

// Run from e2e/run.sh: the only runner is cordoned, so every create must fail
// until it is uncordoned.
test.describe('Runner cordon', () => {
  test.beforeEach(() => {
    test.skip(!runnerCordonConfigured(), 'needs the runner control gRPC address and mTLS material from e2e/run.sh');
  });

  test('a cordoned runner takes no new creates and keeps serving its sandboxes', async () => {
    // Each cordon call is a `go run`, which compiles on first use.
    test.setTimeout(120_000);
    const id = await createSandbox();
    const leaked: string[] = [];
    try {
      expect(runnerCordon('cordon')).toBe(true);
      expect(runnerCordon('cordon-status')).toBe(true);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(1);
      // Well inside the 10s heartbeat interval: the change is sent at once.
      await waitForAdminCordoned(true, 5_000);

      const before = await placements();
      const refused = await postCreate();
      if (refused.body.id) leaked.push(refused.body.id);
      expect(refused.status).toBe(503);
      expect(refused.body.reason).toBe('no_capacity');
      // Placement skipped the runner instead of asking it and being refused.
      expect(await placementsSince(before)).toMatchObject({ no_runner: 1, exhausted: 0 });

      // A stopped sandbox still wakes on a cordoned runner.
      stopSandboxViaRunner(id);
      const r = await execWithTransientRetry(id, "printf '%s' still-served");
      expect(r.stdout.trim()).toBe('still-served');

      expect(runnerCordon('uncordon')).toBe(false);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(0);
      await waitForAdminCordoned(false, 5_000);
      leaked.push(await createSandbox());
    } finally {
      runnerCordon('uncordon');
      await deleteSandbox(id);
      for (const extra of leaked) await deleteSandbox(extra);
    }
  });
});
