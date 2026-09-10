import { randomUUID } from 'node:crypto';
import { test, expect } from '@playwright/test';
import './matchers';
import {
  BASE_URL,
  deleteSandbox,
  execWithTransientRetry,
  getApiKey,
  waitForSandboxStatus,
} from './helpers';

// How long after sending POST /sandboxes the client hangs up. The API hands the
// create to the runner within a few ms, and the runner takes ~0.7-0.9s on both
// lanes, so this lands while the runner is still building the sandbox. If the
// create ever answers first, the test fails rather than passing vacuously; see
// the first assertion.
const DISCONNECT_AFTER_MS = 150;

test.describe('client disconnect during create', () => {
  test('leaves a tracked sandbox, never an untracked one', async ({ request }) => {
    test.setTimeout(120_000);
    const id = randomUUID();
    const apiKey = await getApiKey();

    // A raw fetch with an aborted signal closes the socket, which is what makes
    // the API's request context cancel; the SDK never disconnects mid-create.
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), DISCONNECT_AFTER_MS);
    const started = Date.now();
    let answered: number | undefined;
    try {
      const res = await fetch(`${BASE_URL}/sandboxes`, {
        method: 'POST',
        headers: { 'X-Api-Key': apiKey, 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
        signal: ac.signal,
      });
      answered = res.status;
    } catch (err) {
      if ((err as Error).name !== 'AbortError') throw err;
    } finally {
      clearTimeout(timer);
    }
    const elapsedMs = Date.now() - started;

    try {
      // The API answered before the client hung up: creates are now faster than
      // DISCONNECT_AFTER_MS and this test no longer exercises a mid-create
      // disconnect. Lower the constant.
      expect(
        answered,
        `create answered ${answered} after ${elapsedMs}ms, before the ${DISCONNECT_AFTER_MS}ms disconnect`,
      ).toBeUndefined();

      // The runner finishes the create regardless of the disconnect, so the row
      // must follow: a sandbox the runner has and the API does not is a slot no
      // quota or sweeper can ever reclaim.
      try {
        await waitForSandboxStatus(request, id, 'running', 60_000);
      } catch (err) {
        throw new Error(`aborted create left no row, the runner-side sandbox is untracked: ${String(err)}`);
      }

      // And the row names a live sandbox, not just a record.
      expect(await execWithTransientRetry(id, 'echo tracked')).toHaveSucceeded();
    } finally {
      await deleteSandbox(id);
    }
  });
});
