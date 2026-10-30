import { test, expect } from '@playwright/test';
import './matchers';
import {
  apiRequest,
  client,
  createSandbox,
  execWithTransientRetry,
  waitForSandbox404,
  waitForSandboxStatus,
} from './helpers';
test.describe.configure({ timeout: 300_000 });

test.describe('idle stop / wake / delete', () => {
  test('stop after idle, exec wakes, then row is deleted', async ({ request }) => {
    const id = await createSandbox();

    // Both suites add the 7s activity lag (twice the 1s write interval, plus 5s).
    // Docker idle suite: stop_after=3s, delete_after=10s.
    // Firecracker idle suite: stop_after=3s, delete_after=90s (stop/snapshot can
    // take tens of seconds with a large rootfs; a short delete window races stop).
    await waitForSandboxStatus(request, id, 'stopped', 120_000);

    const execRes = await execWithTransientRetry(id, 'echo wake');
    expect(execRes).toHaveSucceeded();

    // delete_after + buffer + sweep from last activity (wake); FC uses 90s+5s.
    await waitForSandbox404(request, id, 120_000);
  });

  // An exec stream that is still open counts as activity, so a command longer
  // than the idle stop window (3s) does not have its sandbox stopped under it.
  test('a command longer than the idle stop window keeps its sandbox running', async ({
    request,
  }) => {
    const id = await createSandbox();
    try {
      const seen = new Set<string>();
      let watching = true;
      const watch = (async () => {
        while (watching) {
          const res = await apiRequest(request, 'GET', `/sandboxes/${id}`);
          if (res.status === 200) seen.add(((await res.json()) as { status: string }).status);
          await new Promise((r) => setTimeout(r, 250));
        }
      })();
      const execRes = await execWithTransientRetry(id, 'sleep 10 && echo done').finally(() => {
        watching = false;
      });
      await watch;

      expect(execRes).toHaveSucceeded();
      expect(execRes.stdout.trim()).toBe('done');
      expect([...seen]).toEqual(['running']);
    } finally {
      await client.deleteSandbox(id).catch(() => undefined);
    }
  });
});
