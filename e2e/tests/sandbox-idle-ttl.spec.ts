import { test, expect } from '@playwright/test';
import './matchers';
import {
  BASE_URL,
  client,
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
// then goes as usual.
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
    // Once the run ends the sandbox goes. This lane's 10 s delete window is
    // shorter than the 10 s heartbeat that last marked it active, so it may be
    // stopped and deleted between two polls.
    await expect
      .poll(async () => {
        const res = await request.get(`${BASE_URL}/sandboxes/${id}`, { headers: { 'X-Api-Key': key } });
        return res.status() === 404 ? 'deleted' : (await res.json()).status;
      }, { timeout: 120_000, intervals: [1_000] })
      .toMatch(/^(stopped|deleted)$/);
  });

  // A sandbox's own 1 min window, and a stream held open for 90 s: the open
  // stream keeps it running past its window; it stops once the stream ends.
  test('an open execution stream holds a sandbox past its own idle window', async ({ request }) => {
    test.setTimeout(300_000);
    const id = await createSandbox({ idleStopAfter: 60, idleDeleteAfter: 600 });
    const key = await getApiKey();
    const status = async () =>
      (await (await request.get(`${BASE_URL}/sandboxes/${id}`, { headers: { 'X-Api-Key': key } })).json()).status;
    const stream = client.exec(id, { command: 'sleep 90', timeoutMs: 120_000 });
    const start = Date.now();
    while (Date.now() - start < 80_000) {
      expect(await status()).toBe('running');
      await new Promise((r) => setTimeout(r, 5_000));
    }
    expect(await stream).toHaveSucceeded();
    await waitForSandboxStatus(request, id, 'stopped', 120_000);
  });
});
