import { test, expect } from '@playwright/test';
import {
  BASE_URL,
  SCALER_API_KEY,
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  scrapeRunnerMetrics,
  stopSandboxViaRunner,
} from './helpers';
import { placements, placementsSince, postCreate } from './capacity-helpers';
import { parseGauge } from './metrics-helpers';

type AdminRunner = { id: string; cordoned: boolean; last_seen: number };

/** Runners that heartbeated recently, as GET /admin/runners reports them. */
async function liveRunners(): Promise<AdminRunner[]> {
  const res = await fetch(`${BASE_URL}/admin/runners`, { headers: { 'X-Api-Key': SCALER_API_KEY } });
  expect(res.status).toBe(200);
  const body = (await res.json()) as { runners: AdminRunner[] };
  const now = Date.now() / 1000;
  return body.runners.filter((r) => now - r.last_seen < 45);
}

/** Cordons or uncordons a runner through the API with the scaler key. */
async function setCordon(id: string, cordoned: boolean): Promise<boolean> {
  const res = await fetch(`${BASE_URL}/admin/runners/${encodeURIComponent(id)}/cordon`, {
    method: cordoned ? 'PUT' : 'DELETE',
    headers: { 'X-Api-Key': SCALER_API_KEY },
  });
  const body = (await res.json()) as { id: string; cordoned: boolean };
  expect(res.status, JSON.stringify(body)).toBe(200);
  expect(body.id).toBe(id);
  return body.cordoned;
}

async function waitForAdminCordoned(want: boolean, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const runners = await liveRunners();
    if (runners.length > 0 && runners.every((r) => r.cordoned === want)) return;
    if (Date.now() > deadline) {
      throw new Error(`/admin/runners did not report cordoned=${want} within ${timeoutMs}ms: ${JSON.stringify(runners)}`);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
}

// Run from e2e/run.sh or e2e/run-firecracker.sh: every runner is cordoned, so
// every create must fail until they are uncordoned.
test.describe('Runner cordon', () => {
  test.beforeEach(() => {
    test.skip(!process.env.E2E_RUNNER_CONTROL_GRPC_ADDR, 'needs one runner whose control gRPC the test can reach');
  });

  test('a cordoned runner takes no new creates and keeps serving its sandboxes', async () => {
    // Stopping the sandbox is a `go run`, which compiles on first use.
    test.setTimeout(120_000);
    const ids = (await liveRunners()).map((r) => r.id);
    expect(ids.length).toBeGreaterThan(0);
    const id = await createSandbox();
    const leaked: string[] = [];
    try {
      for (const runner of ids) expect(await setCordon(runner, true)).toBe(true);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(1);
      // Well inside the 10s heartbeat interval: the change is sent at once.
      await waitForAdminCordoned(true, 5_000);

      const before = await placements();
      const refused = await postCreate();
      if (refused.body.id) leaked.push(refused.body.id);
      expect(refused.status).toBe(503);
      expect(refused.body.reason).toBe('no_capacity');
      // Placement skipped the runner instead of asking it and being refused.
      expect(await placementsSince(before)).toMatchObject({ no_runner: 1, exhausted: 0 });

      // A stopped sandbox still wakes on a cordoned runner.
      stopSandboxViaRunner(id);
      const r = await execWithTransientRetry(id, "printf '%s' still-served");
      expect(r.stdout.trim()).toBe('still-served');

      for (const runner of ids) expect(await setCordon(runner, false)).toBe(false);
      expect(parseGauge(scrapeRunnerMetrics(), 'sandbox_runner_cordoned')).toBe(0);
      await waitForAdminCordoned(false, 5_000);
      leaked.push(await createSandbox());
    } finally {
      for (const runner of ids) await setCordon(runner, false).catch(() => {});
      await deleteSandbox(id);
      for (const extra of leaked) await deleteSandbox(extra);
    }
  });
});
