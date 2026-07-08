import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import {
  createSandbox,
  deleteSandbox,
  exec,
  restartFirecrackerRunnerFromEnvFile,
  scrapeRunnerMetricsAt,
  stopFirecrackerRunnerPid,
  waitRunnerHttpReady,
} from './helpers';
import { parseGauge } from './metrics-helpers';
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

async function waitDockerRunnerHTTPReady(name: string, deadlineMs: number, pollMs: number): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync(
        'docker',
        ['exec', name, 'wget', '-q', '-O', '-', 'http://localhost:8080/readyz'],
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

async function waitFirecrackerRunnerHostingFirstSandbox(
  addr1: string,
  addr2: string,
): Promise<'1' | '2'> {
  const deadline = Date.now() + 45_000;
  while (Date.now() < deadline) {
    const used1 = parseGauge(scrapeRunnerMetricsAt(addr1), 'sandbox_containers_active');
    const used2 = parseGauge(scrapeRunnerMetricsAt(addr2), 'sandbox_containers_active');
    if (used1 === 1 && used2 === 0) return '1';
    if (used2 === 1 && used1 === 0) return '2';
    await sleep(500);
  }
  throw new Error('could not detect which Firecracker runner hosts the first sandbox');
}

test.describe('API restart resilience', () => {
  test('sandboxes keep working after API container restart', { tag: '@e2e-api-restart' }, async ({ request }) => {
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
  test(
    'stopped runner: 503 on sandboxes there; other runner and new sandboxes still work',
    { tag: '@e2e-stopped-runner' },
    async ({ request }) => {
    const dockerMode =
      process.env.E2E_RUNNER1_CONTAINER_NAME && process.env.E2E_RUNNER2_CONTAINER_NAME;
    const firecrackerMode =
      process.env.E2E_RUNNER1_HTTP_ADDR &&
      process.env.E2E_RUNNER2_HTTP_ADDR &&
      process.env.E2E_RUNNER1_PID &&
      process.env.E2E_RUNNER2_PID;
    test.skip(
      !dockerMode && !firecrackerMode,
      'needs two-runner harness (e2e/run-two-runners.sh or e2e/run-firecracker-two-runners-azure.sh)',
    );
    test.setTimeout(150_000);

    let stoppedRunner = '';
    let stoppedRunnerEnvFile = '';
    const id1 = await createSandbox();
    let id2 = '';

    const firecrackerRemoteForRunner = (runnerKey: '1' | '2') => {
      if (runnerKey !== '2' || !process.env.E2E_RUNNER2_REMOTE_SSH) {
        return undefined;
      }
      return {
        ssh: process.env.E2E_RUNNER2_REMOTE_SSH,
        sshOpts: process.env.E2E_RUNNER2_REMOTE_SSH_OPTS,
      };
    };

    try {
      if (firecrackerMode) {
        const addr1 = process.env.E2E_RUNNER1_HTTP_ADDR!;
        const addr2 = process.env.E2E_RUNNER2_HTTP_ADDR!;
        const deadKey = await waitFirecrackerRunnerHostingFirstSandbox(addr1, addr2);
        id2 = await createSandbox();

        const deadPid = deadKey === '1' ? process.env.E2E_RUNNER1_PID! : process.env.E2E_RUNNER2_PID!;
        stoppedRunnerEnvFile =
          deadKey === '1'
            ? process.env.E2E_RUNNER1_ENV_FILE || ''
            : process.env.E2E_RUNNER2_ENV_FILE || '';

        await exec(id1, `printf '%s' only-on-dead > /tmp/dead-runner-marker`);
        stopFirecrackerRunnerPid(deadPid, firecrackerRemoteForRunner(deadKey));
        stoppedRunner = deadPid;
      } else {
        const runner1 = process.env.E2E_RUNNER1_CONTAINER_NAME!;
        const runner2 = process.env.E2E_RUNNER2_CONTAINER_NAME!;
        id2 = await createSandbox();

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

        await exec(id1, `printf '%s' only-on-dead > /tmp/dead-runner-marker`);
        stoppedRunner = deadRunner;
        docker(['stop', '-t', '30', stoppedRunner]);
      }

      const bad = await request.post(`/sandboxes/${id1}/executions`, {
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
      if (stoppedRunner && firecrackerMode && stoppedRunnerEnvFile) {
        const deadKey =
          stoppedRunner === process.env.E2E_RUNNER1_PID
            ? '1'
            : stoppedRunner === process.env.E2E_RUNNER2_PID
              ? '2'
              : undefined;
        restartFirecrackerRunnerFromEnvFile(
          stoppedRunnerEnvFile,
          deadKey ? firecrackerRemoteForRunner(deadKey) : undefined,
        );
        const deadAddr =
          stoppedRunner === process.env.E2E_RUNNER1_PID
            ? process.env.E2E_RUNNER1_HTTP_ADDR!
            : process.env.E2E_RUNNER2_HTTP_ADDR!;
        await waitRunnerHttpReady(deadAddr);
        await sleep(2000);
      } else if (stoppedRunner && dockerMode) {
        docker(['start', stoppedRunner]);
        try {
          await waitDockerRunnerHTTPReady(stoppedRunner, 75_000, 250);
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
      if (id2) await deleteSandbox(id2);
    }
  });
});
