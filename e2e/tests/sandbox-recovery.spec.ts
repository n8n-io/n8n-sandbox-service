import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createSandbox, deleteSandbox, exec } from './helpers';

function docker(args: string[]): string {
  return execFileSync('docker', args, {
    stdio: ['pipe', 'pipe', 'pipe'],
    encoding: 'utf8',
  });
}

function innerContainerName(sandboxID: string): string {
  return `sandbox-${sandboxID.slice(0, 12)}`;
}

function dockerOutput(args: string[]): string {
  try {
    return execFileSync('docker', args, {
      stdio: ['pipe', 'pipe', 'pipe'],
      encoding: 'utf8',
    });
  } catch (err: unknown) {
    const e = err as { stdout?: unknown; stderr?: unknown };
    const stdout = e.stdout ? String(e.stdout) : '';
    const stderr = e.stderr ? String(e.stderr) : '';
    return `${stdout}\n${stderr}`.trim();
  }
}

function enableOOMGroupIfSupported(runnerContainer: string, innerName: string): boolean {
  const out = dockerOutput([
    'exec',
    runnerContainer,
    'docker',
    'exec',
    '--user',
    '0:0',
    innerName,
    'sh',
    '-c',
    "if [ -w /sys/fs/cgroup/memory.oom.group ]; then echo 1 > /sys/fs/cgroup/memory.oom.group; cat /sys/fs/cgroup/memory.oom.group; else echo UNSUPPORTED; fi",
  ]).trim();
  return out === '1';
}

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

function inspectRestartState(runnerContainer: string, innerName: string): { restartCount: number; running: boolean; oomKilled: boolean } {
  const out = docker([
    'exec',
    runnerContainer,
    'docker',
    'inspect',
    '--format',
    '{{.RestartCount}} {{.State.Running}} {{.State.OOMKilled}}',
    innerName,
  ]).trim();
  const [restartCountRaw, runningRaw, oomKilledRaw] = out.split(/\s+/);
  return {
    restartCount: Number.parseInt(restartCountRaw || '0', 10),
    running: runningRaw === 'true',
    oomKilled: oomKilledRaw === 'true',
  };
}

async function waitContainerRestartedByOOM(
  runnerContainer: string,
  innerName: string,
  baselineRestartCount: number,
  deadlineMs: number,
): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  let lastState = 'unavailable';
  const timeline: string[] = [];
  while (Date.now() < deadline) {
    try {
      const st = inspectRestartState(runnerContainer, innerName);
      lastState = `restartCount=${st.restartCount} running=${st.running} oomKilled=${st.oomKilled}`;
      timeline.push(`${new Date().toISOString()} ${lastState}`);
      // Some Docker/cgroup setups don't keep OOMKilled=true after restart, so the
      // robust signal is restart count increase + running again.
      if (st.restartCount > baselineRestartCount && st.running) {
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
    `container ${innerName} was not observed as OOM-restarted within ${deadlineMs}ms (${lastState})\n\n` +
      `===== state timeline =====\n${timeline.join('\n')}\n\n` +
      `===== docker inspect ${innerName} =====\n${inspectDump}\n\n` +
      `===== docker logs ${innerName} =====\n${logsDump}`,
  );
}

test.describe('Sandbox recovery on runner', () => {
  test('OOM-killed sandbox container is restarted and reachable with same sandbox id', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(150_000);

    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;
    const id = await createSandbox();
    const innerName = innerContainerName(id);
    try {
      const before = inspectRestartState(runnerContainer, innerName);
      // Tighten memory so the allocation loop OOM-kills the container quickly.
      docker(['exec', runnerContainer, 'docker', 'update', '--memory', '64m', '--memory-swap', '64m', innerName]);
      // Restart assertion is only deterministic when we can set memory.oom.group=1:
      // then OOM kills the whole sandbox cgroup (including PID1/daemon), triggering
      // Docker restart policy. Without this support, OOM may kill only the child process.
      test.skip(
        !enableOOMGroupIfSupported(runnerContainer, innerName),
        'cgroup memory.oom.group not supported; cannot deterministically assert container restart on OOM',
      );

      // Trigger memory pressure from inside the sandbox.
      try {
        docker([
          'exec',
          runnerContainer,
          'docker',
          'exec',
          innerName,
          'python3',
          '-c',
          "chunks=[]\nwhile True:\n chunks.append('x'*16*1024*1024)",
        ]);
      } catch {
        // Expected: process/container should be killed by OOM.
      }

      await waitContainerRestartedByOOM(runnerContainer, innerName, before.restartCount, 90_000);
      await waitExecOK(id, `printf '%s' after-oom`, 90_000);
    } finally {
      await deleteSandbox(id);
    }
  });
});
