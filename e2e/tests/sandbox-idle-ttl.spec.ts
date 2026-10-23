import { test, expect } from '@playwright/test';
import './matchers';
import {
  BASE_URL,
  createSandbox,
  execWithTransientRetry,
  getApiKey,
  startAndDisconnect,
  waitForSandbox404,
  waitForSandboxStatus,
} from './helpers';
test.describe.configure({ timeout: 300_000 });

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

// A run in progress is activity: with a 3 s idle-stop window, a sandbox whose
// 25 s execution has lost its stream stays running until the execution ends,
// then stops as usual.
test.describe('keepalive', () => {
  test('a running execution holds the sandbox past its idle window', async ({ request }) => {
    const id = await createSandbox();
    await startAndDisconnect(id, 'sleep 25');
    const key = await getApiKey();
    const status = async () =>
      (await (await request.get(`${BASE_URL}/sandboxes/${id}`, { headers: { 'X-Api-Key': key } })).json()).status;
    const start = Date.now();
    while (Date.now() - start < 18_000) {
      expect(await status()).toBe('running');
      await new Promise((r) => setTimeout(r, 2_000));
    }
    await waitForSandboxStatus(request, id, 'stopped', 120_000);
  });
});
