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
const RECOVERIES = 'sandbox_recoveries_total';
const ACTIVE = 'sandbox_containers_active';
const STOPPED = 'sandbox_containers_stopped';
const RUNNER_LABELS = { role: 'runner' };
const SUCCESS_LABELS = { role: 'runner', result: 'success' };

// A background listener stands in for the long-running processes a client keeps in a
// sandbox — a dev server, a watcher. Detached from the exec that starts it, so it
// outlives that request and is killed only by the crash. Node and curl both ship in
// the sandbox image.
const LISTENER_START =
  `nohup node -e 'require("http").createServer((_, res) => res.end("up")).listen(3000)' ` +
  `> /tmp/listener.log 2>&1 & sleep 1`;
const LISTENER_PROBE = 'curl -sf --max-time 3 http://127.0.0.1:3000';

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

  test('the request that recovers a crashed sandbox is refused with 409, and the retry succeeds', async ({
    request,
  }) => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_HTTP_ADDR from e2e/run-firecracker.sh)',
    );
    test.setTimeout(150_000);

    const before = scrapeRunnerMetrics();
    const deathsBefore = parseCounter(before, GUEST_DEATHS, RUNNER_LABELS);
    const recoveriesBefore = parseCounter(before, RECOVERIES, SUCCESS_LABELS);
    const activeBefore = parseGauge(before, ACTIVE);
    const stoppedBefore = parseGauge(before, STOPPED);

    const id = await createSandboxWithRetry();
    try {
      await crashGuest(id);
      // The 409 is only reachable once the sandbox is marked stopped, which is the
      // last thing the crash handler does. Until then the runner still reports it
      // running and the request fails against a proxy nothing is behind.
      await waitForCrashHandled({
        deaths: deathsBefore + 1,
        active: activeBefore,
        stopped: stoppedBefore + 1,
      });

      const refused = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
        data: { command: 'true' },
      });
      expect(refused.status).toBe(409);
      expect(refused.headers['x-sandbox-restarted']).toBe('1');
      expect((await refused.json()) as { reason?: string }).toMatchObject({
        reason: 'sandbox_restarted',
      });

      // A 409 must not look like a missing sandbox to the API, or it would reap the
      // store row and the sandbox would vanish instead of being usable again.
      const still = await apiRequest(request, 'GET', `/sandboxes/${id}`);
      expect(still.status).toBe(200);

      // The recovery finished before the 409 was written, so this is not a race:
      // the retry has a running sandbox to reach, and gets no second 409.
      const retried = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
        data: { command: `printf '%s' recovered` },
      });
      expect(retried.status).toBe(200);
      expect(retried.headers['x-sandbox-restarted']).toBeUndefined();
      expect(retried.body).toContain('recovered');

      const after = scrapeRunnerMetrics();
      expect(parseCounter(after, RECOVERIES, SUCCESS_LABELS)).toBe(recoveriesBefore + 1);
      expect(parseGauge(after, ACTIVE)).toBe(activeBefore + 1);
      expect(parseGauge(after, STOPPED)).toBe(stoppedBefore);
    } finally {
      await deleteSandbox(id);
    }
  });

  // The two halves of what a recovery is worth, asserted together because they come
  // out of one crash: the disk is why cold boot beats recreating the sandbox, and the
  // lost processes are why the 409 exists at all.
  test('a recovered sandbox keeps its files and loses the processes it was running', async ({
    request,
  }) => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_HTTP_ADDR from e2e/run-firecracker.sh)',
    );
    test.setTimeout(180_000);

    const before = scrapeRunnerMetrics();
    const deathsBefore = parseCounter(before, GUEST_DEATHS, RUNNER_LABELS);
    const activeBefore = parseGauge(before, ACTIVE);
    const stoppedBefore = parseGauge(before, STOPPED);

    const id = await createSandboxWithRetry();
    try {
      // Synced, because the crash is a kernel panic: it takes the guest's page cache
      // with it, and ext4 would not have committed a write this recent on its own.
      // What survives a crash is what reached the disk, so that is what this asserts.
      expect(
        await exec(id, `printf '%s' survives-a-crash > /tmp/crash-marker && sync`),
      ).toHaveSucceeded();
      expect(await exec(id, LISTENER_START)).toHaveSucceeded();
      expect(await exec(id, LISTENER_PROBE)).toHaveSucceeded();

      await crashGuest(id);
      await waitForCrashHandled({
        deaths: deathsBefore + 1,
        active: activeBefore,
        stopped: stoppedBefore + 1,
      });

      const refused = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
        data: { command: 'true' },
      });
      expect(refused.status).toBe(409);

      // Written before the crash, by a guest whose memory is gone: this is the file
      // a recreated sandbox would not have.
      const marker = await exec(id, 'cat /tmp/crash-marker');
      expect(marker).toHaveSucceeded();
      expect(marker.stdout.trim()).toBe('survives-a-crash');

      // Nothing restarts a process that was only in memory. Its files are still
      // there, so a client that gets the 409 can start it again.
      const probe = await exec(id, LISTENER_PROBE);
      expect(probe.exitCode).not.toBe(0);
      expect(await exec(id, LISTENER_START)).toHaveSucceeded();
      expect(await exec(id, LISTENER_PROBE)).toHaveSucceeded();
    } finally {
      await deleteSandbox(id);
    }
  });
});
