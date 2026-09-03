import { test, expect } from '@playwright/test';
import './matchers';
import { apiRequest, client, createSandbox, execWithTransientRetry } from './helpers';
test.describe.configure({ timeout: 300_000 });

test.describe('ephemeral sandbox', () => {
  test('is deleted at the idle-stop window without ever reporting stopped', async ({ request }) => {
    const id = await createSandbox({ ephemeral: true });

    const created = await client.getSandbox(id);
    expect(created.ephemeral).toBe(true);
    expect(created.status).toBe('running');

    // Works like any other sandbox while active.
    const execRes = await execWithTransientRetry(id, 'echo ephemeral');
    expect(execRes).toHaveSucceeded();

    // Idle suite: stop_after=3s, buffer=2s (Docker) / 5s (Firecracker), sweep=1s.
    // The row goes straight from running to gone; the regular idle-delete
    // window (10s / 90s) never comes into play. Poll until 404 and record every
    // status seen on the way: an ephemeral row is never written as "stopped".
    const seen = new Set<string>();
    const deadline = Date.now() + 120_000;
    let gone = false;
    while (Date.now() < deadline) {
      const res = await apiRequest(request, 'GET', `/sandboxes/${id}`);
      if (res.status === 404) {
        gone = true;
        break;
      }
      expect(res.status).toBe(200);
      const j = (await res.json()) as { status: string; ephemeral: boolean };
      expect(j.ephemeral).toBe(true);
      seen.add(j.status);
      await new Promise((r) => setTimeout(r, 250));
    }
    expect(gone).toBe(true);
    expect([...seen]).toEqual(['running']);

    // Nothing to wake: the sandbox is gone for exec too.
    const afterExec = await apiRequest(request, 'POST', `/sandboxes/${id}/executions`, {
      data: { command: 'echo wake' },
    });
    expect(afterExec.status).toBe(404);
  });

  test('regular sandbox created without the flag reports ephemeral false', async () => {
    const id = await createSandbox();
    try {
      const record = await client.getSandbox(id);
      expect(record.ephemeral).toBe(false);
    } finally {
      await client.deleteSandbox(id);
    }
  });
});
