import { test, expect } from '@playwright/test';
import './matchers';
import {
  apiRequest,
  crashGuest,
  createSandboxWithRetry,
  deleteSandbox,
  exec,
  scrapeRunnerMetrics,
} from './helpers';
import { parseCounter, parseGauge } from './metrics-helpers';
import { FIRECRACKER_ONLY } from './tags';

const GUEST_DEATHS = 'sandbox_guest_deaths_total';
const ACTIVE = 'sandbox_containers_active';
const STOPPED = 'sandbox_containers_stopped';
const RUNNER_LABELS = { role: 'runner' };

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

type CrashState = { deaths: number; active: number; stopped: number };

function crashHandled(body: string, want: CrashState): boolean {
  return (
    parseCounter(body, GUEST_DEATHS, RUNNER_LABELS) >= want.deaths &&
    parseGauge(body, ACTIVE) === want.active &&
    parseGauge(body, STOPPED) === want.stopped
  );
}

/**
 * Waits for the runner to notice the guest is gone and finish reacting to it, and
 * returns the scrape it settled on so a caller can assert further on those values.
 *
 * The death counter is deliberately not the only gate. It is incremented before the
 * handler has even taken the sandbox's transition claim, and the sandbox is marked
 * stopped and its slot handed back only after the microVM has been torn down, so a
 * gate on the counter alone can return while active/stopped still hold their
 * pre-crash values. Both gauges are evaluated at scrape time, which makes them the
 * first honest sign that the crash is fully handled.
 *
 * Every condition throws here rather than being left to the caller. Callers treat
 * this as a precondition, so returning with the crash half-handled would let a test
 * run on against a sandbox the runner never finished stopping and fail later on
 * something unrelated — which is the regression these tests exist to catch, reported
 * as a puzzle. Each condition names what the runner failed to do.
 */
async function waitForCrashHandled(want: CrashState, timeoutMs = 60_000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  let body = scrapeRunnerMetrics();
  while (!crashHandled(body, want) && Date.now() < deadline) {
    await sleep(500);
    body = scrapeRunnerMetrics();
  }

  const deaths = parseCounter(body, GUEST_DEATHS, RUNNER_LABELS);
  if (deaths < want.deaths) {
    throw new Error(
      `${GUEST_DEATHS} reached ${deaths}, want >= ${want.deaths} within ${timeoutMs}ms — ` +
        'the guest may not have died, or the runner did not detect it',
    );
  }
  const active = parseGauge(body, ACTIVE);
  const stopped = parseGauge(body, STOPPED);
  if (active !== want.active || stopped !== want.stopped) {
    throw new Error(
      `${ACTIVE} = ${active} (want ${want.active}) and ${STOPPED} = ${stopped} ` +
        `(want ${want.stopped}) within ${timeoutMs}ms — the runner counted the death but ` +
        'never finished handling it: the sandbox was not marked stopped, or its slot was ' +
        'not released',
    );
  }
  return body;
}

// Docker detects a dead guest through its own event stream and heals it via the
// container restart policy, so these assertions are Firecracker-specific until
// that lands.
test.describe('Guest crash detection', FIRECRACKER_ONLY, () => {
  test('a dead guest gives its slot back and leaves the sandbox stopped', async () => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_HTTP_ADDR from e2e/run-firecracker.sh)',
    );
    test.setTimeout(150_000);

    const before = scrapeRunnerMetrics();
    const deathsBefore = parseCounter(before, GUEST_DEATHS, RUNNER_LABELS);
    const activeBefore = parseGauge(before, ACTIVE);
    const stoppedBefore = parseGauge(before, STOPPED);

    const id = await createSandboxWithRetry();
    try {
      expect(await exec(id, `printf '%s' alive`)).toHaveSucceeded();
      expect(parseGauge(scrapeRunnerMetrics(), ACTIVE)).toBe(activeBefore + 1);

      await crashGuest(id);

      // The point of tearing down eagerly: a crashed sandbox costs disk and
      // nothing else, so a client retrying cannot accumulate slots.
      const after = await waitForCrashHandled({
        deaths: deathsBefore + 1,
        active: activeBefore,
        stopped: stoppedBefore + 1,
      });
      expect(parseGauge(after, ACTIVE)).toBe(activeBefore);
      expect(parseGauge(after, STOPPED)).toBe(stoppedBefore + 1);
    } finally {
      // Deleting a crashed sandbox has to succeed: it is already torn down, so a
      // delete that tried to pause a dead microVM would fail on every attempt.
      await deleteSandbox(id);
    }
  });

  test('a crashed sandbox is refused rather than restored from its stale snapshot', async ({
    request,
  }) => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_HTTP_ADDR from e2e/run-firecracker.sh)',
    );
    test.setTimeout(150_000);

    const before = scrapeRunnerMetrics();
    const deathsBefore = parseCounter(before, GUEST_DEATHS, RUNNER_LABELS);
    const activeBefore = parseGauge(before, ACTIVE);
    const stoppedBefore = parseGauge(before, STOPPED);

    const id = await createSandboxWithRetry();
    try {
      await exec(id, `printf '%s' marker > /tmp/crash-marker`);

      await crashGuest(id);
      // The refusal below is only reachable once the sandbox is marked stopped,
      // which is the last thing the crash handler does. Until then the runner still
      // reports it running and the request fails against a proxy nothing is behind.
      await waitForCrashHandled({
        deaths: deathsBefore + 1,
        active: activeBefore,
        stopped: stoppedBefore + 1,
      });

      // Restoring the snapshot here would corrupt the rootfs the guest kept
      // writing to, so the request must fail loudly instead of appearing to
      // succeed against a stale disk. PR 4 turns this into a 409 once the
      // sandbox can be cold-booted back.
      const resp = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
        data: { command: 'true' },
      });
      expect(resp.status).toBe(503);
      expect(resp.body.toLowerCase()).toContain('crashed');

      // A 503 must not look like a missing sandbox to the API, or it would reap
      // the store row and the sandbox would vanish instead of being recoverable.
      const still = await apiRequest(request, 'GET', `/sandboxes/${id}`);
      expect(still.status).toBe(200);
    } finally {
      await deleteSandbox(id);
    }
  });
});
