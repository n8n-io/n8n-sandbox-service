import { test, expect, type APIRequestContext } from '@playwright/test';
import './matchers';
import {
  apiRequest,
  BASE_URL,
  client,
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  getApiKey,
  startAndDisconnect,
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

    // Both suites add the 2s activity lag (twice the 1s write interval), and
    // deletes wait it twice.
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

// Every runner is cordoned, so this has to run where nothing else creates
// sandboxes at the same time: workers is 1.
test.describe('stopped sandbox on a cordoned runner', () => {
  test('is deleted by the request that would wake it, and can be created again', async ({ request }) => {
    const runners = (await liveRunners()).map((runner) => runner.id);
    expect(runners.length).toBeGreaterThan(0);
    const id = await createSandbox();
    try {
      // Must land before the idle fence (delete_after, 10s on Docker, plus twice
      // the activity write interval), or the fence answers the 404 instead.
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
