import { test, expect } from '@playwright/test';
import {
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  runnerCordon,
  runnerCordonConfigured,
  scrapeRunnerMetrics,
  stopSandboxViaRunner,
} from './helpers';
import { postCreate } from './capacity-helpers';
import { parseGauge } from './metrics-helpers';

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

      const refused = await postCreate();
      if (refused.body.id) leaked.push(refused.body.id);
      expect(refused.status).toBe(503);
      expect(refused.body.reason).toBe('no_capacity');

      // A stopped sandbox still wakes on a cordoned runner.
      stopSandboxViaRunner(id);
      const r = await execWithTransientRetry(id, "printf '%s' still-served");
      expect(r.stdout.trim()).toBe('still-served');

      expect(runnerCordon('uncordon')).toBe(false);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(0);
      leaked.push(await createSandbox());
    } finally {
      runnerCordon('uncordon');
      await deleteSandbox(id);
      for (const extra of leaked) await deleteSandbox(extra);
    }
  });
});
