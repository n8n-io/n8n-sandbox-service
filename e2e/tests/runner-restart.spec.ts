import { execFileSync } from 'node:child_process';
import { test, expect } from '@playwright/test';
import { apiRequest, createSandbox, deleteSandbox, exec, restartRunnerForE2E, waitRunnerHttpReady } from './helpers';
async function waitDockerRunnerReady(container: string, deadlineMs = 75_000): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync(
        'docker',
        [
          'exec',
          container,
          'wget',
          '-q',
          '-O',
          '-',
          '--no-check-certificate',
          'https://localhost:8080/readyz',
        ],
        { stdio: 'pipe' },
      );
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 250));
    }
  }
  throw new Error(`runner container ${container} not ready within ${deadlineMs}ms`);
}

test.describe('Runner restart', () => {
  test('sandboxes are unavailable after runner restart', async ({ request }) => {
    test.setTimeout(120_000);

    const id = await createSandbox();
    try {
      const ok = await exec(id, `printf '%s' marker`);
      expect(ok.exitCode).toBe(0);

      restartRunnerForE2E();
      if (process.env.E2E_RUNNER_HTTP_ADDR) {
        await waitRunnerHttpReady(process.env.E2E_RUNNER_HTTP_ADDR);
      } else if (process.env.E2E_RUNNER_CONTAINER_NAME) {
        await waitDockerRunnerReady(process.env.E2E_RUNNER_CONTAINER_NAME);
      }

      // The restarted runner no longer knows the sandbox and says so with
      // X-Sandbox-Gone, the only signal the API drops its row on. Polled because
      // the runner may still answer 502 or 503 while it settles.
      await expect
        .poll(
          async () =>
            (await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, { data: { command: 'true' } }))
              .status,
          { timeout: 30_000, intervals: [500] },
        )
        .toBe(404);
      expect((await apiRequest(request, 'GET', `/sandboxes/${id}`)).status).toBe(404);
    } finally {
      await deleteSandbox(id).catch(() => undefined);
    }
  });
});
