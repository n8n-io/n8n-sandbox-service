import { test, expect } from '@playwright/test';
import './matchers';
import {
  createSandbox,
  execWithTransientRetry,
  waitForSandbox404,
  waitForSandboxStatus,
} from './helpers';
test.describe.configure({ timeout: 180_000 });

test.describe('idle stop / wake / delete', () => {
  test('stop after idle, exec wakes, then row is deleted', async ({ request }) => {
    const id = await createSandbox();

    // run.sh / run-firecracker-idle-ttl.sh: stop_after=3s, sweep=1s.
    // Firecracker stop (pause + snapshot) can take much longer than Docker stop.
    await waitForSandboxStatus(request, id, 'stopped', 120_000);

    const execRes = await execWithTransientRetry(id, 'echo wake');
    expect(execRes).toHaveSucceeded();

    // delete_after=10s, buffer=2s, sweep=1s from last activity (wake)
    await waitForSandbox404(request, id, 25_000);
  });
});
