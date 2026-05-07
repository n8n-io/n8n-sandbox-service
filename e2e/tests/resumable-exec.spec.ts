import { test, expect } from '@playwright/test';
import { client, createSandbox, deleteSandbox, startAndDisconnect } from './helpers';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

test.describe('Resumable Exec', () => {
  let sandboxId: string;

  test.beforeEach(async () => {
    sandboxId = await createSandbox();
  });

  test.afterEach(async () => {
    await deleteSandbox(sandboxId);
  });

  test('disconnect does not kill command — full output recoverable via resume', async () => {
    test.setTimeout(15_000);

    const execId = await startAndDisconnect(
      sandboxId,
      'echo before && sleep 2 && echo after',
    );
    expect(execId).toBeTruthy();

    await sleep(2500);

    const result = await client.resumeExecSession(sandboxId, execId, 0);

    expect(result.stdout).toContain('before');
    expect(result.stdout).toContain('after');
    expect(result.exitCode).toBe(0);
    expect(result.killed).toBe(false);
  });

  test('cancel kills running command', async () => {
    test.setTimeout(15_000);

    const execId = await startAndDisconnect(sandboxId, 'sleep 30');
    expect(execId).toBeTruthy();

    await client.cancelExecSession(sandboxId, execId);

    const result = await client.resumeExecSession(sandboxId, execId, 0);

    expect(result.killed).toBe(true);
  });
});
