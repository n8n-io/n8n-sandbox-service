import { test, expect } from '@playwright/test';
import './matchers';
import { apiRequest, client, createSandbox, execWithTransientRetry } from './helpers';
test.describe.configure({ timeout: 300_000 });

type ListedSandbox = { id: string; status: string; ephemeral: boolean };

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
    // GET /sandboxes/{id} is fenced (404) as soon as the stop window passes; the
    // unfenced list keeps the row until the sweeper has deleted it, which is what
    // proves cleanup. The fence must be up before the row goes, and the row is
    // never "stopped".
    const seen = new Set<string>();
    const deadline = Date.now() + 120_000;
    let fenced = false;
    let removed = false;
    while (Date.now() < deadline) {
      const list = await apiRequest(request, 'GET', '/sandboxes');
      expect(list.status).toBe(200);
      const row = ((await list.json()) as ListedSandbox[]).find((r) => r.id === id);
      if (!row) {
        removed = true;
        break;
      }
      expect(row.ephemeral).toBe(true);
      seen.add(row.status);

      const res = await apiRequest(request, 'GET', `/sandboxes/${id}`);
      if (res.status === 404) {
        fenced = true;
      } else {
        expect(res.status).toBe(200);
        expect(fenced, 'fence must not drop once up').toBe(false);
      }
      await new Promise((r) => setTimeout(r, 250));
    }
    expect(removed, 'sweeper never removed the ephemeral row').toBe(true);
    expect(fenced, 'row was removed without the fence going up first').toBe(true);
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
