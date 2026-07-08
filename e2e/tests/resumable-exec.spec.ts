import { test, expect } from '@playwright/test';
import './matchers';
import { SandboxServiceError } from '@n8n/sandbox-client';
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

    const result = await client.resumeExecution(sandboxId, execId, 0);

    expect(result.stdout).toContain('before');
    expect(result.stdout).toContain('after');
    expect(result).toHaveSucceeded();
    expect(result.killed).toBe(false);
  });

  test('delete kills running command and removes execution state', async () => {
    test.setTimeout(15_000);

    const execId = await startAndDisconnect(sandboxId, 'sleep 30');
    expect(execId).toBeTruthy();

    await client.deleteExecution(sandboxId, execId);

    const err = await client.resumeExecution(sandboxId, execId, 0).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err).toMatchObject({
      status: 404,
      message: 'execution not found',
    });
  });
});
