import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, execWithTransientRetry, scrapeRunnerMetrics } from './helpers';
import { placements, placementsSince, postCreate } from './capacity-helpers';
import { liveRunners, setCordon, waitForAdminCordoned } from './cordon-helpers';
import { parseGauge } from './metrics-helpers';

// Run from e2e/run.sh or e2e/run-firecracker.sh: every runner is cordoned, so
// every create must fail until they are uncordoned. A stopped sandbox on a
// cordoned runner is covered in sandbox-idle-ttl.spec.ts, where the API stops it.
test.describe('Runner cordon', () => {
  test.beforeEach(() => {
    test.skip(!process.env.E2E_RUNNER_CONTROL_GRPC_ADDR, 'needs the single-runner lane, whose runner metrics the test scrapes');
  });

  test('a cordoned runner takes no new creates and keeps serving its running sandboxes', async () => {
    const ids = (await liveRunners()).map((runner) => runner.id);
    expect(ids.length).toBeGreaterThan(0);
    const id = await createSandbox();
    const leaked: string[] = [];
    try {
      for (const runner of ids) expect(await setCordon(runner, true)).toBe(true);
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

      const served = await execWithTransientRetry(id, "printf '%s' still-served");
      expect(served.stdout.trim()).toBe('still-served');

      for (const runner of ids) expect(await setCordon(runner, false)).toBe(false);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(0);
      await waitForAdminCordoned(false, 5_000);
      leaked.push(await createSandbox());
    } finally {
      for (const runner of ids) await setCordon(runner, false).catch(() => {});
      await deleteSandbox(id);
      for (const extra of leaked) await deleteSandbox(extra);
    }
  });
});
