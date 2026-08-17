import { expect, test } from '@playwright/test';
import {
  createSandbox,
  deleteSandbox,
  exec,
  execWithTransientRetry,
  scrapeRunnerMetrics,
  stopSandboxViaRunner,
  waitForSandbox404,
} from './helpers';
import { parseGauge } from './metrics-helpers';
import { FIRECRACKER_ONLY } from './tags';

test.describe.configure({ timeout: 120_000 });

// Firecracker-only: that runner serializes create/stop/wake/delete per sandbox so
// a delete cannot tear down around an in-flight wake. The Docker runtime has not
// been audited for the same overlap, so this stays off the Docker lane rather than
// putting an unvalidated concurrency assertion on the required PR check.
test.describe('delete during wake', FIRECRACKER_ONLY, () => {
  test('delete racing a wake completes and leaves no slot behind', async ({ request }) => {
    const activeGauge = 'sandbox_containers_active';
    const stoppedGauge = 'sandbox_containers_stopped';
    const beforeBody = scrapeRunnerMetrics();
    const activeBefore = parseGauge(beforeBody, activeGauge);
    const stoppedBefore = parseGauge(beforeBody, stoppedGauge);

    const id = await createSandbox();
    await execWithTransientRetry(id, 'echo before-stop');
    stopSandboxViaRunner(id);

    // The exec triggers a wake and the delete lands while it is in flight. Either
    // may win, so the exec outcome is deliberately not asserted; what matters is
    // that the delete settles and the runner is left clean. A leaked transition
    // claim would instead hang the delete until the test timeout.
    const results = await Promise.allSettled([exec(id, 'echo racing-wake'), deleteSandbox(id)]);
    const deleteOutcome = results[1];
    const deleteReason = deleteOutcome.status === 'rejected' ? String(deleteOutcome.reason) : '';
    expect(deleteOutcome.status, `delete during wake failed: ${deleteReason}`).toBe('fulfilled');

    await waitForSandbox404(request, id);

    // Once the delete returns, no lifecycle operation for this sandbox can still
    // be running, so a slot or tracked state left behind shows up as a gauge above
    // its baseline. Compared against baselines rather than zero because other
    // sandboxes may be alive on this runner, and as an upper bound rather than
    // equality so an unrelated sandbox being reaped mid-test cannot fail the test.
    const afterBody = scrapeRunnerMetrics();
    expect(parseGauge(afterBody, activeGauge)).toBeLessThanOrEqual(activeBefore);
    expect(parseGauge(afterBody, stoppedGauge)).toBeLessThanOrEqual(stoppedBefore);
  });
});
