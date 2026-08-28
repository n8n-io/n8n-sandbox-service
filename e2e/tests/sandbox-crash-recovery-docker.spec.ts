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
import { DOCKER_ONLY } from './tags';

const GUEST_DEATHS = 'sandbox_guest_deaths_total';
const RECOVERIES = 'sandbox_recoveries_total';
const ACTIVE = 'sandbox_containers_active';
const RUNNER_LABELS = { role: 'runner' };
const SUCCESS_LABELS = { role: 'runner', result: 'success' };

// Stands in for the long-running processes a client keeps in a sandbox — a dev
// server, a watcher. Detached from the exec that starts it, so only the crash ends it.
const LISTENER_START =
  `nohup node -e 'require("http").createServer((_, res) => res.end("up")).listen(3000)' ` +
  `> /tmp/listener.log 2>&1 & sleep 1`;
const LISTENER_PROBE = 'curl -sf --max-time 3 http://127.0.0.1:3000';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Waits until the runner has read the container's death off Docker's event stream.
 *
 * Unlike Firecracker, nothing about the sandbox settles into a stopped state to wait
 * for: Docker's restart policy has the container back up, usually before this even
 * returns. The death counter is the honest gate here because the runner marks the
 * sandbox restarted before incrementing it, so once the counter moves the next request
 * is guaranteed to meet the 409 rather than race the event.
 */
async function waitForDeath(want: number, timeoutMs = 60_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (parseCounter(scrapeRunnerMetrics(), GUEST_DEATHS, RUNNER_LABELS) >= want) {
      return;
    }
    await sleep(500);
  }
  throw new Error(
    `${GUEST_DEATHS} did not reach ${want} within ${timeoutMs}ms — the container may not ` +
      'have died, or the runner is not watching docker events',
  );
}

// Docker restarts a died container on its own, without telling anyone. These tests
// cover the runner noticing, and turning a silent restart into the same contract a
// Firecracker recovery gets.
test.describe('Docker guest crash recovery', DOCKER_ONLY, () => {
  test('a container Docker restarted is refused once with 409, then usable with its files', async ({
    request,
  }) => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_CONTAINER_NAME from e2e/run.sh)',
    );
    test.setTimeout(150_000);

    const before = scrapeRunnerMetrics();
    const deathsBefore = parseCounter(before, GUEST_DEATHS, RUNNER_LABELS);
    const recoveriesBefore = parseCounter(before, RECOVERIES, SUCCESS_LABELS);
    const activeBefore = parseGauge(before, ACTIVE);

    const id = await createSandboxWithRetry();
    try {
      expect(await exec(id, `printf '%s' survives-a-crash > /tmp/crash-marker`)).toHaveSucceeded();
      expect(await exec(id, LISTENER_START)).toHaveSucceeded();
      expect(await exec(id, LISTENER_PROBE)).toHaveSucceeded();

      await crashGuest(id);
      await waitForDeath(deathsBefore + 1);

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

      // The retry proves both halves of the repair: the sandbox answers at all, which
      // it only does once its network rules follow the restarted container, and it is
      // not refused a second time for the same crash.
      const retried = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
        data: { command: `printf '%s' recovered` },
      });
      expect(retried.status).toBe(200);
      expect(retried.headers['x-sandbox-restarted']).toBeUndefined();
      expect(retried.body).toContain('recovered');

      // The container's writable layer is why a restart beats recreating the sandbox.
      const marker = await exec(id, 'cat /tmp/crash-marker');
      expect(marker).toHaveSucceeded();
      expect(marker.stdout.trim()).toBe('survives-a-crash');

      // And what the 409 exists to report: nothing brings back a process that was only
      // in memory, so the client has to start it again.
      expect((await exec(id, LISTENER_PROBE)).exitCode).not.toBe(0);
      expect(await exec(id, LISTENER_START)).toHaveSucceeded();
      expect(await exec(id, LISTENER_PROBE)).toHaveSucceeded();

      const after = scrapeRunnerMetrics();
      expect(parseCounter(after, RECOVERIES, SUCCESS_LABELS)).toBe(recoveriesBefore + 1);
      // The container never left, so unlike Firecracker no slot is given back.
      expect(parseGauge(after, ACTIVE)).toBe(activeBefore + 1);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('a sandbox the runner shuts down is not reported as a crash', async () => {
    test.skip(
      !process.env.E2E_RUNNER_HTTP_ADDR && !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs runner metrics (E2E_RUNNER_CONTAINER_NAME from e2e/run.sh)',
    );
    test.setTimeout(120_000);

    const deathsBefore = parseCounter(scrapeRunnerMetrics(), GUEST_DEATHS, RUNNER_LABELS);

    const id = await createSandboxWithRetry();
    try {
      expect(await exec(id, `printf '%s' alive`)).toHaveSucceeded();
      await deleteSandbox(id);

      // Docker reports the death of a running container the runner removed exactly as it
      // reports a crash; only the runner knows which it asked for. Reading this one as a
      // crash would count a guest death on every delete.
      await sleep(3_000);
      expect(parseCounter(scrapeRunnerMetrics(), GUEST_DEATHS, RUNNER_LABELS)).toBe(deathsBefore);
    } finally {
      // The delete above is the action under test, not cleanup, so on the happy path
      // this second one is a 404 the helper swallows. It earns its place when an
      // assertion before it throws: this runner has one slot per sandbox and a leaked
      // container would starve every spec that follows.
      await deleteSandbox(id);
    }
  });
});
