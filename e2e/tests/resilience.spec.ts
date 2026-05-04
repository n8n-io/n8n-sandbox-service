import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createSandbox, deleteSandbox, exec } from './helpers';

const API_KEY = process.env.SANDBOX_API_KEY || 'test';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function docker(args: string[]): void {
  execFileSync('docker', args, { stdio: 'inherit' });
}

function dockerOutput(args: string[]): string {
  try {
    return execFileSync('docker', args, { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
  } catch (err: unknown) {
    const e = err as { stdout?: unknown; stderr?: unknown };
    const stdout = e.stdout ? String(e.stdout) : '';
    const stderr = e.stderr ? String(e.stderr) : '';
    return `${stdout}\n${stderr}`.trim();
  }
}

/** Poll until success or deadline; fast poll so we exit as soon as the service is up (not 1s granularity). */
async function waitForAPI(request: APIRequestContext, deadlineMs: number, pollMs: number): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      const h = await request.get('/healthz');
      if (h.ok()) return;
    } catch {
      /* API not listening yet */
    }
    await sleep(pollMs);
  }
  throw new Error(`API did not become healthy within ${deadlineMs}ms`);
}

async function waitRunnerHTTPReady(name: string, apiKey: string, deadlineMs: number, pollMs: number): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync(
        'docker',
        ['exec', name, 'wget', '-q', '-O', '-', '--header', `X-Api-Key: ${apiKey}`, 'http://localhost:8080/healthz'],
        { stdio: 'pipe' },
      );
      return;
    } catch {
      /* runner process or dockerd not ready */
    }
    await sleep(pollMs);
  }
  throw new Error(`Runner ${name} HTTP not ready within ${deadlineMs}ms`);
}

async function waitInnerDockerReady(
  runnerContainer: string,
  deadlineMs: number,
  pollMs: number,
): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync('docker', ['exec', runnerContainer, 'docker', 'info'], { stdio: 'pipe' });
      return;
    } catch {
      /* inner dockerd still starting */
    }
    await sleep(pollMs);
  }
  throw new Error(`inner docker on ${runnerContainer} not ready within ${deadlineMs}ms`);
}

/** Matches internal/runner/manager/manager.go: "sandbox-" + sandboxID[:12] */
function sandboxInnerContainerName(sandboxUUID: string): string {
  return `sandbox-${sandboxUUID.slice(0, 12)}`;
}

function runnerHostListsSandbox(runnerContainer: string, sandboxUUID: string): boolean {
  const want = sandboxInnerContainerName(sandboxUUID);
  try {
    const out = execFileSync(
      'docker',
      ['exec', runnerContainer, 'docker', 'ps', '-a', '--format', '{{.Names}}'],
      { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] },
    );
    return out
      .split(/\r?\n/)
      .map((l: string) => l.trim())
      .some((line: string) => line === want);
  } catch {
    return false;
  }
}

test.describe('API restart resilience', () => {
  test('sandboxes keep working after API container restart', async ({ request }) => {
    test.skip(!process.env.E2E_API_CONTAINER_NAME, 'needs E2E_API_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(100_000);

    const apiContainer = process.env.E2E_API_CONTAINER_NAME!;

    const id = await createSandbox();
    try {
      await exec(id, `printf '%s' ok > /tmp/api-restart-marker`);

      docker(['restart', apiContainer]);

      await waitForAPI(request, 75_000, 250);
      await sleep(3000);

      const r = await exec(id, 'cat /tmp/api-restart-marker');
      expect(r.stdout.trim()).toBe('ok');
    } finally {
      await deleteSandbox(id);
    }
  });
});

test.describe('Runner failure resilience', () => {
  test('stopped runner: 503 on sandboxes there; other runner and new sandboxes still work', async ({ request }) => {
    test.skip(
      !process.env.E2E_RUNNER1_CONTAINER_NAME || !process.env.E2E_RUNNER2_CONTAINER_NAME,
      'needs E2E_RUNNER1_CONTAINER_NAME and E2E_RUNNER2_CONTAINER_NAME (from e2e/run-two-runners.sh)',
    );
    // Align with runner bootstrap bounds on restart (dockerd wait can take up to 60s).
    test.setTimeout(150_000);

    const runner1 = process.env.E2E_RUNNER1_CONTAINER_NAME!;
    const runner2 = process.env.E2E_RUNNER2_CONTAINER_NAME!;
    const runnerKey = process.env.E2E_RUNNER_INTERNAL_API_KEY || 'runner-test';

    const id1 = await createSandbox();
    const id2 = await createSandbox();

    // gRPC registration order is not guaranteed (either runner may heartbeat first), so do not
    // assume id1 maps to runner1. Detect which host actually runs each sandbox's inner container.
    let id1On1 = false;
    let id1On2 = false;
    for (let i = 0; i < 30; i++) {
      id1On1 = runnerHostListsSandbox(runner1, id1);
      id1On2 = runnerHostListsSandbox(runner2, id1);
      if (id1On1 !== id1On2) break;
      await sleep(500);
    }
    expect(id1On1 !== id1On2).toBe(true);
    const deadRunner = id1On1 ? runner1 : runner2;
    const liveRunner = id1On1 ? runner2 : runner1;

    expect(runnerHostListsSandbox(liveRunner, id2)).toBe(true);
    expect(runnerHostListsSandbox(deadRunner, id2)).toBe(false);

    let stoppedRunner = '';
    try {
      await exec(id1, `printf '%s' only-on-r1 > /tmp/dead-runner-marker`);

      stoppedRunner = deadRunner;
      docker(['stop', stoppedRunner]);

      const bad = await request.post(`/sandboxes/${id1}/exec`, {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/json' },
        data: { command: 'true' },
      });
      expect(bad.status()).toBe(503);
      const errBody = (await bad.json()) as { error?: string };
      expect(errBody.error?.toLowerCase()).toContain('runner unavailable');

      const good = await exec(id2, `printf '%s' alive`);
      expect(good.stdout.trim()).toBe('alive');

      const id3 = await createSandbox();
      try {
        const r3 = await exec(id3, `printf '%s' new`);
        expect(r3.stdout.trim()).toBe('new');
      } finally {
        await deleteSandbox(id3);
      }
    } finally {
      if (stoppedRunner) {
        docker(['start', stoppedRunner]);
        try {
          // start-runner.sh waits for inner dockerd (up to 60s) before sandbox-runner starts listening.
          await waitRunnerHTTPReady(stoppedRunner, runnerKey, 75_000, 250);
          await waitInnerDockerReady(stoppedRunner, 45_000, 250);
        } catch (err) {
          const logs = dockerOutput(['logs', stoppedRunner]);
          throw new Error(
            `runner restart readiness failed for ${stoppedRunner}: ${String(err)}\n\n` +
              `===== docker logs ${stoppedRunner} =====\n${logs}`,
          );
        }
        await sleep(2000);
      }
      await deleteSandbox(id1);
      await deleteSandbox(id2);
    }
  });
});
