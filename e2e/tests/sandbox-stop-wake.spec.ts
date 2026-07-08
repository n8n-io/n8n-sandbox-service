import { expect, test } from '@playwright/test';
import './matchers';
import {
  apiRequest,
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  stopSandboxViaRunner,
} from './helpers';
test.describe.configure({ timeout: 120_000 });

test.describe('direct stop / wake', () => {
  test('runner stop then exec wakes sandbox', async ({ request }) => {
    const id = await createSandbox();
    try {
      await execWithTransientRetry(id, 'echo before-stop');

      stopSandboxViaRunner(id);

      const execRes = await execWithTransientRetry(id, 'echo after-wake');
      expect(execRes).toHaveSucceeded();
      expect(execRes.stdout.trim()).toBe('after-wake');

      const getRunning = await apiRequest(request, 'GET', `/sandboxes/${id}`);
      expect(getRunning.status).toBe(200);
      const running = (await getRunning.json()) as { status?: string };
      expect(running.status).toBe('running');
    } finally {
      await deleteSandbox(id);
    }
  });
});
