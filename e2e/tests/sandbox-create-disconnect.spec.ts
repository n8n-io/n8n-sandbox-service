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

// Delays (ms) between sending POST /sandboxes and hanging up, tried in order.
// The hang-up has to happen while the runner is still building the sandbox.
// Creates take ~0.7-0.9s, so 150ms is normally enough; if the API answers
// before the hang-up, the next shorter delay is tried. Below 20ms the hang-up
// may land before the API has even called the runner, so if every delay is
// answered the test fails rather than pretending it tested anything.
const DISCONNECT_OFFSETS_MS = [150, 75, 40, 20];

// POST /sandboxes and hang up after afterMs. Uses fetch rather than the SDK
// because aborting a fetch closes the socket, which is what cancels the API's
// request context. Returns the status if the API answered first, undefined if
// the hang-up won.
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

    // Find a delay that hangs up mid-create. A 201 means the sandbox was created
    // and must be cleaned up; any other status is an unrelated failure.
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
        `the API answered before every hang-up delay (${answers.join(', ')}): ` +
          'creates are too fast for this test to hang up mid-create',
      );
    }

    try {
      // The runner finishes the create despite the hang-up, so the API must
      // store the row too. A sandbox the runner has but the API does not know
      // about holds a slot forever: quota and the idle sweeper never see it.
      try {
        await waitForSandboxStatus(request, id, 'running', 60_000);
      } catch (err) {
        throw new Error(
          `no sandbox after hanging up at ${disconnectAfterMs}ms: either the runner-side sandbox ` +
            `is untracked, or the hang-up landed before the API called the runner: ${String(err)}`,
        );
      }

      // The sandbox is live, not just a row.
      expect(await execWithTransientRetry(id, 'echo tracked')).toHaveSucceeded();
    } finally {
      await deleteSandbox(id);
    }
  });
});
