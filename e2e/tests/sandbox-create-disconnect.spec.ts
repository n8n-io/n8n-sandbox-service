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

// How long after sending POST /sandboxes the client hangs up, tried largest
// first. The API hands the create to the runner within a few ms and the runner
// takes ~0.7-0.9s on both lanes, so the first offset lands mid-create. If the
// create answers first, the next attempt hangs up earlier rather than failing
// on a speedup. Below the last offset the API's own pre-runner work (auth, id
// lock) is a comparable share of the request, and a hang-up that lands before
// the runner is asked is indistinguishable from the bug this guards, so the
// test gives up and says so instead of passing vacuously.
const DISCONNECT_OFFSETS_MS = [150, 75, 40, 20];

// A raw fetch with an aborted signal closes the socket, which is what makes the
// API's request context cancel; the SDK never disconnects mid-create. Resolves
// to the status if the API answered first, undefined if the client hung up first.
async function createAndHangUp(apiKey: string, id: string, afterMs: number): Promise<number | undefined> {
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), afterMs);
  try {
    const res = await fetch(`${BASE_URL}/sandboxes`, {
      method: 'POST',
      headers: { 'X-Api-Key': apiKey, 'Content-Type': 'application/json' },
      body: JSON.stringify({ id }),
      signal: ac.signal,
    });
    return res.status;
  } catch (err) {
    if ((err as Error).name !== 'AbortError') throw err;
    return undefined;
  } finally {
    clearTimeout(timer);
  }
}

test.describe('client disconnect during create', () => {
  test('leaves a tracked sandbox, never an untracked one', async ({ request }) => {
    test.setTimeout(120_000);
    const apiKey = await getApiKey();

    // Find an offset that hangs up mid-create. An answered 201 is a real sandbox
    // that has to go; any other answer is an infrastructure failure, not a miss.
    let id: string | undefined;
    let disconnectAfterMs = 0;
    const answers: string[] = [];
    for (const afterMs of DISCONNECT_OFFSETS_MS) {
      const candidate = randomUUID();
      const started = Date.now();
      const answered = await createAndHangUp(apiKey, candidate, afterMs);
      if (answered === undefined) {
        id = candidate;
        disconnectAfterMs = afterMs;
        break;
      }
      const elapsedMs = Date.now() - started;
      if (answered !== 201) {
        throw new Error(`create answered ${answered} after ${elapsedMs}ms`);
      }
      await deleteSandbox(candidate);
      answers.push(`${elapsedMs}ms < ${afterMs}ms`);
    }
    if (id === undefined) {
      throw new Error(
        `create answered before every disconnect offset (${answers.join(', ')}): ` +
          'creates are now too fast for a client to hang up mid-create, so this test no longer exercises one',
      );
    }

    try {
      // The runner finishes the create regardless of the disconnect, so the row
      // must follow: a sandbox the runner has and the API does not is a slot no
      // quota or sweeper can ever reclaim.
      try {
        await waitForSandboxStatus(request, id, 'running', 60_000);
      } catch (err) {
        throw new Error(
          `create aborted at ${disconnectAfterMs}ms left no row: the runner-side sandbox is untracked, ` +
            `or the hang-up landed before the API reached the runner: ${String(err)}`,
        );
      }

      // And the row names a live sandbox, not just a record.
      expect(await execWithTransientRetry(id, 'echo tracked')).toHaveSucceeded();
    } finally {
      await deleteSandbox(id);
    }
  });
});
