import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, execWithTransientRetry, scrapeRunnerMetrics, stopSandboxViaRunner } from './helpers';
import { parseCounter } from './metrics-helpers';
import { BOTH_RUNNERS } from './tags';

test.describe('Runner stop/wake metrics', BOTH_RUNNERS, () => {
  test('records stop and ensure_running operations', async () => {
    test.setTimeout(120_000);

    const containerOps = 'sandbox_container_operations_total';
    const stopLabels = { role: 'runner', operation: 'stop', result: 'success' };
    const wakeLabels = { role: 'runner', operation: 'ensure_running', result: 'success' };
    const stoppedGauge = 'sandbox_containers_stopped';

    const beforeBody = scrapeRunnerMetrics();
    const stopsBefore = parseCounter(beforeBody, containerOps, stopLabels);
    const wakesBefore = parseCounter(beforeBody, containerOps, wakeLabels);

    const id = await createSandbox();
    try {
      stopSandboxViaRunner(id);

      const stoppedBody = scrapeRunnerMetrics();
      const stopsAfterStop = parseCounter(stoppedBody, containerOps, stopLabels);
      expect(stopsAfterStop).toBeGreaterThanOrEqual(stopsBefore + 1);
      expect(stoppedBody).toContain(stoppedGauge);

      await execWithTransientRetry(id, 'echo metrics-wake');
      const afterWakeBody = scrapeRunnerMetrics();
      const wakesAfterWake = parseCounter(afterWakeBody, containerOps, wakeLabels);
      expect(wakesAfterWake).toBeGreaterThanOrEqual(wakesBefore + 1);
    } finally {
      await deleteSandbox(id);
    }
  });
});
