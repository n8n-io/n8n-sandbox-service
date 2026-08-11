import { test, expect } from '@playwright/test';
import './matchers';
import {
  createSandbox,
  execWithTransientRetry,
  waitForSandbox404,
  waitForSandboxStatus,
} from './helpers';
test.describe.configure({ timeout: 300_000 });

test.describe('idle stop / wake / delete', () => {
  test('stop after idle, exec wakes, then row is deleted', async ({ request }) => {
    const id = await createSandbox();

    // Docker idle suite: stop_after=3s, delete_after=10s.
    // Firecracker idle suite: stop_after=3s, delete_after=90s (stop/snapshot can
    // take tens of seconds with a large rootfs; a short delete window races stop).
    await waitForSandboxStatus(request, id, 'stopped', 120_000);

    const execRes = await execWithTransientRetry(id, 'echo wake');
    expect(execRes).toHaveSucceeded();

    // delete_after + buffer + sweep from last activity (wake); FC uses 90s+5s.
    await waitForSandbox404(request, id, 120_000);
  });
});
