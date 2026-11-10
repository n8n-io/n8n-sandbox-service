import { test, expect, type APIRequestContext } from '@playwright/test';
import './matchers';
import {
  apiRequest,
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  waitForSandbox404,
  waitForSandboxStatus,
} from './helpers';
import { liveRunners, setCordon, waitForAdminCordoned } from './cordon-helpers';
import { parseCounter } from './metrics-helpers';
test.describe.configure({ timeout: 300_000 });

async function cordonDeletes(request: APIRequestContext): Promise<number> {
  const resp = await request.get('/metrics');
  expect(resp.status()).toBe(200);
  return parseCounter(await resp.text(), 'sandbox_sandbox_operations_total', {
    operation: 'cordon_delete',
    result: 'success',
  });
}

test.describe('idle stop / wake / delete', () => {
  test('stop after idle, exec wakes, then row is deleted', async ({ request }) => {
    const id = await createSandbox();

    // Docker idle suite: stop_after=3s, delete_after=10s.
    // Firecracker idle suite: stop_after=3s, delete_after=90s (stop/snapshot can
    // take tens of seconds with a large rootfs; a short delete window races stop).
    await waitForSandboxStatus(request, id, 'stopped', 120_000);

    const execRes = await execWithTransientRetry(id, 'echo wake');
    expect(execRes).toHaveSucceeded();

    // delete_after + buffer + sweep from last activity (wake); FC uses 90s+5s.
    await waitForSandbox404(request, id, 120_000);
  });
});

// Every runner is cordoned, so this has to run where nothing else creates
// sandboxes at the same time: workers is 1.
test.describe('stopped sandbox on a cordoned runner', () => {
  test('is deleted by the request that would wake it, and can be created again', async ({ request }) => {
    const runners = (await liveRunners()).map((runner) => runner.id);
    expect(runners.length).toBeGreaterThan(0);
    const id = await createSandbox();
    try {
      // Must land before delete_after (10s on Docker), or the idle fence answers
      // the 404 instead.
      await waitForSandboxStatus(request, id, 'stopped', 120_000);
      for (const runner of runners) expect(await setCordon(runner, true)).toBe(true);
      await waitForAdminCordoned(true, 5_000);
      const deletesBefore = await cordonDeletes(request);

      // The 404 and the missing row are what make n8n create the sandbox again.
      const gone = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, { data: { command: 'true' } });
      expect(gone.status, gone.body).toBe(404);
      expect((await apiRequest(request, 'GET', `/sandboxes/${id}`)).status).toBe(404);
      expect(await cordonDeletes(request)).toBe(deletesBefore + 1);

      for (const runner of runners) expect(await setCordon(runner, false)).toBe(false);
      await waitForAdminCordoned(false, 5_000);
      expect(await createSandbox({ id })).toBe(id);
      const recreated = await execWithTransientRetry(id, "printf '%s' recreated");
      expect(recreated.stdout.trim()).toBe('recreated');
    } finally {
      for (const runner of runners) await setCordon(runner, false).catch(() => {});
      await deleteSandbox(id);
    }
  });
});
