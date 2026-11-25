import { randomUUID } from 'node:crypto';
import { test, expect } from '@playwright/test';
import { SandboxServiceError, type CodeTaskEvent } from '@n8n/sandbox-client';
import { adminClient } from './helpers';
import { DOCKER_ONLY } from './tags';

// Runs against the e2e-only code image (e2e/code-runtime). Its runtime runs
// the `code` of task.json as an async function body with `require` and
// `console`, and puts {result, console} in result.json. e2e/run.sh enables
// code sandboxes on the runner and sets the limits checked here.

const CPU_PERCENT = Number(process.env.E2E_CODE_CPU_PERCENT || '50');
const MAX_MEMORY_MB = Number(process.env.E2E_CODE_MAX_MEMORY_MB || '1024');
const MEMORY_MB = 384;

const TASK_TIMEOUT_MS = 30_000;

const client = adminClient();
const created: string[] = [];

async function createCodeSandbox(memoryMb = MEMORY_MB): Promise<string> {
  const sandbox = await client.createCodeSandbox({
    name: `e2e-${randomUUID()}`,
    runtime: 'n8n-js',
    runtimeVersion: '0.0.0-e2e',
    maxConcurrency: 1,
    memoryMb,
  });
  expect(sandbox.created).toBe(true);
  created.push(sandbox.id);
  return sandbox.id;
}

async function runTask(sandboxId: string, code: string): Promise<CodeTaskEvent> {
  const taskId = `t_${randomUUID().replace(/-/g, '')}`;
  const events = await client.runCodeTask(sandboxId, {
    spec: { taskId, timeoutMs: TASK_TIMEOUT_MS },
    taskInput: JSON.stringify({ code }),
  });
  for await (const event of events) {
    if (event.type === 'done' || event.type === 'error') {
      expect(event.taskId).toBe(taskId);
      return event;
    }
  }
  throw new Error('task stream ended without a terminal event');
}

// Reads the sandbox's cgroup limits, on cgroup v2 or v1.
const READ_LIMITS = `
  const fs = require('node:fs');
  const read = (path) => { try { return fs.readFileSync(path, 'utf8').trim(); } catch { return null; } };
  const memoryMax = read('/sys/fs/cgroup/memory.max');
  if (memoryMax !== null) {
    const [quota, period] = read('/sys/fs/cgroup/cpu.max').split(' ');
    return { memoryBytes: Number(memoryMax), cpuQuota: Number(quota), cpuPeriod: Number(period) };
  }
  return {
    memoryBytes: Number(read('/sys/fs/cgroup/memory/memory.limit_in_bytes')),
    cpuQuota: Number(read('/sys/fs/cgroup/cpu/cpu.cfs_quota_us')),
    cpuPeriod: Number(read('/sys/fs/cgroup/cpu/cpu.cfs_period_us')),
  };
`;

test.describe('code sandboxes', DOCKER_ONLY, () => {
  test.skip(!process.env.E2E_CODE_SANDBOXES, 'needs a runner with code sandboxes enabled (e2e/run.sh)');
  test.describe.configure({ timeout: 180_000 });

  test.afterEach(async () => {
    for (const id of created.splice(0)) await client.deleteCodeSandbox(id);
  });

  test('runs a task and returns its result and console output', async () => {
    const id = await createCodeSandbox();

    const event = await runTask(id, 'console.log("hello", 1); return 40 + 2;');
    expect(event).toMatchObject({ type: 'done', result: { result: 42, console: ['hello 1'] } });
  });

  test('reports a task that throws as a user error', async () => {
    const id = await createCodeSandbox();

    const event = await runTask(id, 'console.log("before"); throw new Error("boom");');
    expect(event).toMatchObject({
      type: 'error',
      code: 'user_error',
      userError: { message: 'boom' },
      result: { console: ['before'] },
    });
  });

  test('applies memoryMb and the runner CPU limit', async () => {
    const id = await createCodeSandbox();

    const event = await runTask(id, READ_LIMITS);
    expect(event.type).toBe('done');
    const limits = (event as { result: { result: { memoryBytes: number; cpuQuota: number; cpuPeriod: number } } })
      .result.result;
    expect(limits.memoryBytes).toBe(MEMORY_MB * 1024 * 1024);
    expect(limits.cpuQuota / limits.cpuPeriod).toBeCloseTo(CPU_PERCENT / 100, 2);
  });

  for (const memoryMb of [63, MAX_MEMORY_MB + 1]) {
    test(`refuses memoryMb ${memoryMb}, outside the runner range`, async () => {
      const error = await createCodeSandbox(memoryMb).catch((e: unknown) => e);
      expect(error).toBeInstanceOf(SandboxServiceError);
      expect((error as SandboxServiceError).status).toBe(400);
    });
  }
});
