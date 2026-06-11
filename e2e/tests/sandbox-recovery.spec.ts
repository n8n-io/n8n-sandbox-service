import { expect, test } from '@playwright/test';
import './matchers';
import { createSandbox, deleteSandbox, docker, exec, innerContainerName } from './helpers';
import { RUNNER_TAGS } from './tags';

async function waitExecOK(sandboxID: string, command: string, deadlineMs: number): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      const out = await exec(sandboxID, command);
      if (out.exitCode === 0) {
        return;
      }
      lastErr = new Error(`non-zero exit code ${out.exitCode}`);
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`sandbox ${sandboxID} did not recover in ${deadlineMs}ms: ${String(lastErr)}`);
}

function inspectRestartState(
  runnerContainer: string,
  innerName: string,
): { restartCount: number; running: boolean; oomKilled: boolean; startedAt: string } {
  const out = docker([
    'exec',
    runnerContainer,
    'docker',
    'inspect',
    '--format',
    '{{.RestartCount}} {{.State.Running}} {{.State.OOMKilled}} {{.State.StartedAt}}',
    innerName,
  ]).trim();
  const [restartCountRaw, runningRaw, oomKilledRaw, startedAtRaw] = out.split(/\s+/);
  return {
    restartCount: Number.parseInt(restartCountRaw || '0', 10),
    running: runningRaw === 'true',
    oomKilled: oomKilledRaw === 'true',
    startedAt: startedAtRaw || '',
  };
}

async function waitContainerRestarted(
  runnerContainer: string,
  innerName: string,
  baseline: { restartCount: number; startedAt: string },
  deadlineMs: number,
): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  let lastState = 'unavailable';
  const timeline: string[] = [];
  while (Date.now() < deadline) {
    try {
      const st = inspectRestartState(runnerContainer, innerName);
      lastState = `restartCount=${st.restartCount} running=${st.running} oomKilled=${st.oomKilled} startedAt=${st.startedAt}`;
      timeline.push(`${new Date().toISOString()} ${lastState}`);
      if (st.running && (st.restartCount > baseline.restartCount || st.startedAt != baseline.startedAt)) {
        return;
      }
    } catch {
      // transient inspect failures while container is restarting
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  const inspectDump = dockerOutput([
    'exec',
    runnerContainer,
    'docker',
    'inspect',
    innerName,
  ]);
  const logsDump = dockerOutput([
    'exec',
    runnerContainer,
    'docker',
    'logs',
    '--tail',
    '200',
    innerName,
  ]);
  throw new Error(
    `container ${innerName} was not observed as restarted within ${deadlineMs}ms (${lastState})\n\n` +
      `===== state timeline =====\n${timeline.join('\n')}\n\n` +
      `===== docker inspect ${innerName} =====\n${inspectDump}\n\n` +
      `===== docker logs ${innerName} =====\n${logsDump}`,
  );
}

test.describe('Sandbox recovery on runner', { tag: RUNNER_TAGS.docker }, () => {
  test('sandbox container restart keeps same sandbox id reachable', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(150_000);

    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;
    const id = await createSandbox();
    const innerName = innerContainerName(id);
    try {
      const before = inspectRestartState(runnerContainer, innerName);
      // Force a full container restart from runner-side Docker and verify the
      // sandbox ID remains usable afterwards.
      docker([
        'exec',
        runnerContainer,
        'docker',
        'restart',
        '--time',
        '0',
        innerName,
      ]);

      await waitContainerRestarted(runnerContainer, innerName, before, 90_000);
      await waitExecOK(id, `printf '%s' after-restart`, 90_000);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('exec succeeds after inner container is stopped (runner auto-wake)', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(60_000);

    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;
    const id = await createSandbox();
    const innerName = innerContainerName(id);
    try {
      docker(['exec', runnerContainer, 'docker', 'stop', '--time', '0', innerName]);
      // Runner proxy wakes the inner container instead of returning 502.
      await waitExecOK(id, `printf '%s' after-stop-wake`, 45_000);
      const st = inspectRestartState(runnerContainer, innerName);
      expect(st.running).toBe(true);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('second exec after stop still succeeds (wake is stable)', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(60_000);

    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;
    const id = await createSandbox();
    const innerName = innerContainerName(id);
    try {
      docker(['exec', runnerContainer, 'docker', 'stop', '--time', '0', innerName]);
      await waitExecOK(id, 'echo first', 45_000);
      const out = await exec(id, 'echo second');
      expect(out).toHaveSucceeded();
      expect(out.stdout.trim()).toBe('second');
    } finally {
      await deleteSandbox(id);
    }
  });
});
