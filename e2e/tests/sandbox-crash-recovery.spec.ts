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

function guestDeaths(): number {
  return parseCounter(scrapeRunnerMetrics(), GUEST_DEATHS, RUNNER_LABELS);
}

/**
 * Waits for the runner to notice the guest is gone. The guest panics a second
 * after the daemon exits, and the runner then tears the microVM down, so this is
 * the point from which the sandbox's state is settled.
 */
async function waitForGuestDeaths(want: number, timeoutMs = 60_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let last = 0;
  while (Date.now() < deadline) {
    last = guestDeaths();
    if (last >= want) return;
    await sleep(500);
  }
  throw new Error(
    `${GUEST_DEATHS} reached ${last}, want >= ${want} within ${timeoutMs}ms — ` +
      'the guest may not have died, or the runner did not detect it',
  );
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
      await waitForGuestDeaths(deathsBefore + 1);

      // The point of tearing down eagerly: a crashed sandbox costs disk and
      // nothing else, so a client retrying cannot accumulate slots.
      const after = scrapeRunnerMetrics();
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

    const deathsBefore = guestDeaths();
    const id = await createSandboxWithRetry();
    try {
      await exec(id, `printf '%s' marker > /tmp/crash-marker`);

      await crashGuest(id);
      await waitForGuestDeaths(deathsBefore + 1);

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
