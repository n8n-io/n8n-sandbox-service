import { test, expect } from '@playwright/test';
import './matchers';
import { apiRequest, createSandbox, execWithTransientRetry } from './helpers';

test.describe.configure({ timeout: 75_000 });

test.describe('idle stop / wake / delete', { tag: '@docker-runner' }, () => {
  test('stop after idle, exec wakes, then row is deleted', async ({ request }) => {
    const id = await createSandbox();

    // run.sh: stop_after=3s, sweep=1s
    await new Promise((r) => setTimeout(r, 5_500));

    const get1 = await apiRequest(request, 'GET', `/sandboxes/${id}`);
    expect(get1.status).toBe(200);
    const j1 = (await get1.json()) as { status?: string };
    expect(j1.status).toBe('stopped');

    const execRes = await execWithTransientRetry(id, 'echo wake');
    expect(execRes).toHaveSucceeded();

    // delete_after=10s, buffer=2s, sweep=1s from last activity (wake)
    await new Promise((r) => setTimeout(r, 16_000));

    const get2 = await apiRequest(request, 'GET', `/sandboxes/${id}`);
    expect(get2.status).toBe(404);
  });
});
